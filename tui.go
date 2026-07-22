package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// enableOutputProcessing re-enables OPOST | ONLCR after term.MakeRaw, which
// turns them off. Without this, '\n' moves the cursor down but not back to
// column 0, breaking every multi-line render.
func enableOutputProcessing(fd int) {
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return
	}
	t.Oflag |= unix.OPOST | unix.ONLCR
	_ = unix.IoctlSetTermios(fd, ioctlSetTermios, t)
}

// marqueeInterval is the frame period for scrolling overflowing DIR cells.
// Currently unused: marquee animation is disabled (see the render closure in
// RunTUI) and trimmed cells render statically at step 0.
const marqueeInterval = 300 * time.Millisecond

// Key constants for arrow keys and Esc, shared with the inputDecoder in
// tui_events.go (which returns them alongside its own KeyEnter/KeyHome/… set).
const (
	KeyUp    = "\x00up"
	KeyDown  = "\x00down"
	KeyLeft  = "\x00left"
	KeyRight = "\x00right"
	KeyEsc   = "\x00esc"
)

// readModalEvents waits for key input or one of the modal's allowed wake
// sources. The caller owns the persistent decoder so split escape sequences and
// lone Esc flushes survive successive modal redraws. Mouse-only input remains
// ignored; it cannot dismiss or redraw a modal without a wake source.
func readModalEvents(dec *inputDecoder, wakes []wakeFD) ([]string, wakeKind) {
	for {
		events, woke := pollEvents(dec, 0, wakes)
		var keys []string
		for _, ev := range events {
			if ev.kind == eventKey {
				keys = append(keys, ev.key)
			}
		}
		if len(keys) > 0 || woke != wakeNone {
			return keys, woke
		}
	}
}

// inspectorChromeRows is the number of fixed rows RenderInspector reserves
// around the scrolling body (title, metadata, separator, footer). The viewport
// height is the terminal height minus this, and must match the body arithmetic
// in RenderInspector.
const inspectorChromeRows = 4

// RunTUI is the live view: alt-screen, raw mode, mouse reporting, and a single
// event loop owning two screens — the session list and the fullscreen
// inspector. Returns nil on clean quit (q / Ctrl-C / Ctrl-D), or an error if
// setup failed.
func RunTUI(interval time.Duration) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)
	// Re-enable output processing so '\n' still translates to '\r\n'.
	enableOutputProcessing(fd)

	// Enable mouse reporting, then alt-screen, hide cursor, disable line-wrap.
	// All restored on return (mouse off first, mirroring the setup order in
	// reverse).
	writeMouseMode(os.Stdout, true)
	fmt.Print("\033[?1049h\033[?25l\033[?7l")
	defer func() {
		writeMouseMode(os.Stdout, false)
		fmt.Print("\033[?7h\033[?25h\033[?1049l")
	}()

	viewMode := LoadViewMode()
	sortMode := LoadSortMode()
	var local []Session
	var remotes []RemoteResult
	var targets []selectionTarget

	// Remote fetches run in a background goroutine so the render loop never
	// blocks on a slow/unreachable host (the per-host HTTP timeout is 5s,
	// which would otherwise freeze the UI for that long every tick). Each
	// host's row populates as its reply arrives — locals paint immediately
	// and remotes stream in independently.
	hub, err := NewRemoteHub(interval)
	if err != nil {
		return fmt.Errorf("init remote hub: %w", err)
	}
	defer hub.Shutdown()

	// Account usage bars: same non-blocking pattern as the remote hub. The
	// first paint happens with no bar; it appears once the initial fetch
	// lands (no wake pipe — the next tick repaints anyway).
	usageHub := NewUsageHub()
	defer usageHub.Shutdown()

	hostUsageHub := NewHostUsageHub(interval)
	defer hostUsageHub.Shutdown()
	localName := shortHostname()

	// Resize handling: a SIGWINCH-driven wake pipe lets a blocked pollEvents
	// return so we redraw at the new size. One goroutine translates the signal
	// to a pipe write and never touches stdin (single-consumer invariant).
	rw, err := newResizeWake()
	if err != nil {
		return fmt.Errorf("init resize wake: %w", err)
	}
	defer rw.Close()
	resizeSignals := make(chan os.Signal, 1)
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	stopResize := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopResize:
				return
			case <-resizeSignals:
				rw.Signal()
			}
		}
	}()
	// Teardown order: stop signal delivery, unblock the goroutine, then (via
	// the earlier defer, which runs last) close the pipe.
	defer func() {
		signal.Stop(resizeSignals)
		close(stopResize)
	}()

	decoder := newInputDecoder()
	state := newTUIState()
	screen := newScreenRenderer(os.Stdout)

	// inspectorHub polls the previewed session while the inspector screen is
	// open; nil on the session list. Shut down on exit if still open.
	var inspectorHub *InspectorHub
	defer func() {
		if inspectorHub != nil {
			inspectorHub.Shutdown()
		}
	}()

	// toast is a transient one-liner (the sort mode after pressing 's') pinned
	// to the terminal's bottom row until toastUntil; the main loop caps its
	// wait at the deadline so the line vanishes on time.
	var toast string
	var toastUntil time.Time

	// settleRows sorts the latest local and remote snapshots, then reconciles
	// selection. It chases a pending post-spawn landing until its tmux pane
	// appears, otherwise falling back if a vanished selected row needs replacing.
	settleRows := func() {
		SortSessions(local, sortMode)
		// Snapshot() returns the hub's shared slices; sort remotes on copies so
		// we never race the hub goroutine that owns them.
		remotes = sortRemotes(hub.Snapshot(), sortMode)
		targets = buildSelectionTargets(local, remotes)
		state.settleSelection(targets)
	}

	// refresh re-reads local sessions through the authoritative loopback server
	// when available (falling back to direct collection), then settles the latest
	// remote snapshot. When kickRemote is true, the hub is also asked to refetch
	// ASAP (used after actions and the 'r' key). Wall-clock ticks pass false
	// because the hub has its own ticker — kicking on every tick would just
	// double-fetch.
	refresh := func(kickRemote bool) {
		if sessions, err := collectClientLocal(); err == nil {
			local = sessions
		}
		if kickRemote {
			hub.Refresh()
		}
		settleRows()
	}

	// markInspectorEndedIfGone flags the inspector as ended when the session it
	// is watching has dropped out of the freshly-refreshed target list, so the
	// view stops reading as live even before the hub's own next poll notices.
	// The render overlay keeps the last content on screen.
	markInspectorEndedIfGone := func() {
		if inspectorHub == nil {
			return
		}
		if findSelectionTarget(targets, state.inspector.targetID) == nil {
			state.inspectorTargetGone = true
		}
	}

	// render paints the active screen. On the session list it builds the table
	// frame, keeps the selected row visible, reserves the bottom row for a footer
	// or active toast, and crops to the terminal viewport (recording hit regions
	// for mouse routing). On the inspector it applies the latest hub snapshot,
	// sizes the viewport, and lets RenderInspector draw + report its controls.
	//
	// Wrap is disabled (?7l): clipLine/cropTableFrame cut each line to the
	// terminal width so an overflowing line can't smear the last column.
	// Marquee animation stays disabled (step 0).
	render := func() {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}

		if state.mode == screenInspector {
			if inspectorHub != nil {
				state.inspector.applySnapshot(inspectorHub.Snapshot())
			}
			if state.inspectorTargetGone {
				// Overlay a terminal "ended" verdict that survives snapshot
				// re-application; content (Lines) is untouched.
				state.inspector.snapshot.Ended = true
				state.inspector.snapshot.Loading = false
				state.inspector.snapshot.Stale = false
			}
			state.inspector.resize(rows - inspectorChromeRows)
			var buf strings.Builder
			state.hits = RenderInspector(&buf, state.inspector, cols, rows)
			_ = screen.Draw(buf.String(), cols, rows)
			return
		}

		frame := BuildTableFrame(viewMode, LocalHost{
			Name:      localName,
			Sessions:  local,
			HostUsage: hostUsageHub.Snapshot(),
		}, remotes, state.sel, usageHub.Snapshot(), cols, 0, sortMode)
		toastActive := rows > 0 && time.Now().Before(toastUntil)
		viewRows := rows
		if rows > 0 {
			viewRows--
		}
		if viewRows < 0 {
			viewRows = 0
		}
		// Free-scroll model: wheel moves listOffset and the selection may leave
		// the viewport; resolveListOffset only re-anchors the view to the
		// selection when a selection change requested it, otherwise it just
		// clamps the current offset.
		state.resolveListOffset(frame, viewRows)

		var out string
		if cols <= 0 {
			// Unknown width: cropTableFrame has no cols<=0 guard, so render
			// uncropped like clipLines does for an unknown terminal size.
			state.hits = nil
			out = strings.Join(frame.lines, "\n")
		} else {
			visible := cropTableFrame(frame, state.listOffset, viewRows, cols)
			state.hits = visible.hits
			out = visible.text
		}
		if rows > 0 {
			out = withBottomRow(
				out,
				rows,
				sessionBottomRow(toast, toastActive),
			)
		}
		_ = screen.Draw(out, cols, rows)
	}

	// Modal screens only listen for resize wakes. Remote and inspector wakes
	// remain owned by the main loop so background data never changes a modal.
	modalWakes := []wakeFD{{fd: rw.FD(), kind: wakeResize}}
	makeCtx := func() *actCtx {
		return &actCtx{
			fd:         fd,
			oldState:   oldState,
			targets:    targets,
			sel:        state.sel,
			modalWakes: modalWakes,
			pause: func() {
				hub.Pause()
				usageHub.Pause()
				hostUsageHub.Pause()
			},
			resume: func() {
				hub.Resume()
				usageHub.Resume()
				hostUsageHub.Resume()
			},
		}
	}

	// openInspector enters the fullscreen inspector for the selected session.
	// Empty-host placeholder rows have no session and are ignored. The hub is
	// built from a private copy of the target so a later list refresh can't
	// mutate what it polls.
	openInspector := func() {
		target := findSelectionTarget(targets, state.sel)
		if target == nil || target.session == nil {
			return
		}
		sess := *target.session
		tcopy := selectionTarget{id: target.id, host: target.host, session: &sess}
		ih, err := NewInspectorHub(tcopy, interval)
		if err != nil {
			return
		}
		inspectorHub = ih
		state.mode = screenInspector
		state.inspector = newInspectorViewState(target.id)
		state.inspectorTargetGone = false
		screen.Invalidate()
		render()
	}

	// closeInspector tears the hub down (which closes its wake fd — so nil the
	// reference before the next pollEvents rebuilds the wakes slice), resets the
	// inspector state, and returns to a freshly-refreshed session list.
	closeInspector := func() {
		if inspectorHub != nil {
			inspectorHub.Shutdown()
			inspectorHub = nil
		}
		state.mode = screenSessions
		state.inspector = inspectorViewState{}
		state.inspectorTargetGone = false
		refresh(false)
		screen.Invalidate()
		render()
	}

	refresh(false)
	render()

	// Wall-clock auto-refresh: tick every `interval` regardless of input.
	// pollEvents takes the time remaining until the next tick; if it returns
	// empty and unwoken, the tick fired and we refresh + advance.
	nextTick := time.Now().Add(interval)

	for {
		timeout := time.Until(nextTick)
		// While a toast is showing, wake at its deadline so the bottom line
		// clears on time. toastTick marks a wait capped for that reason: its
		// expiry repaints only, leaving the wall-clock cadence untouched.
		toastTick := false
		if until := time.Until(toastUntil); until > 0 && until < timeout {
			timeout = until
			toastTick = true
		}
		if timeout <= 0 {
			refresh(false)
			render()
			nextTick = time.Now().Add(interval)
			continue
		}

		// Rebuild the wakes slice each iteration: the inspector hub comes and
		// goes, and its fd must never be polled after Shutdown closed it.
		wakes := []wakeFD{
			{fd: hub.WakeFD(), kind: wakeRemote},
			{fd: rw.FD(), kind: wakeResize},
		}
		if inspectorHub != nil {
			wakes = append(wakes, wakeFD{fd: inspectorHub.WakeFD(), kind: wakeInspector})
		}
		events, woke := pollEvents(decoder, timeout, wakes)

		if len(events) == 0 {
			switch {
			case woke == wakeNone:
				// Timed out. A toast deadline that expired before the wall
				// clock repaints only (render drops the expired toast);
				// otherwise the wall-clock tick fired.
				if toastTick && time.Now().Before(nextTick) {
					render()
					continue
				}
				refresh(false)
				render()
				nextTick = time.Now().Add(interval)
			case woke&wakeRemote != 0:
				// Remote data landed: refresh locals + list and re-render. This
				// also resets the wall clock so the hub ticker and this loop
				// don't double-render every cycle and drift.
				refresh(false)
				markInspectorEndedIfGone()
				render()
				nextTick = time.Now().Add(interval)
			default:
				// Resize and/or inspector update only: redraw at the current
				// size (render re-reads it) without disturbing the wall clock.
				render()
			}
			continue
		}

		if woke&wakeRemote != 0 {
			// Stdin and a remote update fired together: refresh so key handlers
			// see the latest snapshot (e.g. nav uses the fresh list).
			refresh(false)
			markInspectorEndedIfGone()
		}
		for _, ev := range events {
			if state.mode == screenInspector {
				if handleInspectorEvent(ev, state, &inspectorHub, closeInspector, render) {
					return nil
				}
				continue
			}
			if ev.kind == eventMouse {
				switch state.handleListMouse(ev.mouse, time.Now()) {
				case commandOpenInspector:
					openInspector()
				case commandRender:
					render()
				}
				continue
			}
			k := ev.key
			if sessionKeyCommand(k) == commandOpenInspector {
				openInspector()
				continue
			}
			switch k {
			case "q", "Q", "\x03", "\x04":
				return nil
			case KeyUp:
				state.navigate(targets, -1)
				render()
			case KeyDown:
				state.navigate(targets, 1)
				render()
			case "k", "K":
				screen.Invalidate()
				actKill(makeCtx())
				refresh(true)
				render()
			case "a", "A":
				screen.Invalidate()
				actAttach(makeCtx())
				refresh(true)
				render()
			case "d", "D":
				ctx := makeCtx()
				update, err := actToggleDisabled(ctx)
				if err != nil {
					screen.Invalidate()
					showActionError(ctx, "disable toggle failed", err)
					render()
					continue
				}
				if update == nil {
					continue
				}
				if update.Host == "" {
					patchDisabledBySessionID(local, update.SessionID, update.Disabled)
				} else {
					hub.PatchDisabled(update.Host, update.SessionID, update.Disabled)
					hub.Refresh()
				}
				settleRows()
				state.requestSelectionAnchor()
				render()
			case "n", "N":
				screen.Invalidate()
				ctx := makeCtx()
				actNew(ctx)
				// Record the spawned session's landing target before refreshing so
				// settleSelection can chase it across refreshes: new local metadata
				// lags and the first remote snapshot is stale, so a one-shot lookup
				// here would miss. Only a real spawn (non-empty tmux) sets pending;
				// a cancelled or failed new-session leaves any prior intent intact.
				if ctx.spawnedTmux != "" {
					state.pending = &pendingSpawn{host: ctx.spawnedHost, tmux: ctx.spawnedTmux}
				}
				refresh(true)
				render()
			case "m", "M":
				switch viewMode {
				case "1":
					viewMode = "3"
				case "3":
					viewMode = "2"
				default:
					viewMode = "1"
				}
				SaveViewMode(viewMode)
				render()
			case "s", "S":
				delta := 1 // s cycles forward, shift-s backward
				if k == "S" {
					delta = -1
				}
				sortMode = cycleSortMode(sortMode, delta)
				SaveSortMode(sortMode)
				toast = "sort: " + sortDesc(sortMode)
				toastUntil = time.Now().Add(4 * time.Second)
				refresh(false)
				render()
			case "r", "R":
				usageHub.Kick()
				hostUsageHub.Kick()
				refresh(true)
				render()
			case "?":
				screen.Invalidate()
				helpDecoder := newInputDecoder()
				for {
					cols, rows, err := term.GetSize(fd)
					if err != nil {
						cols, rows = 0, 0
					}
					_ = screen.Draw(renderHelp(sortMode), cols, rows)
					keys, _ := readModalEvents(helpDecoder, modalWakes)
					if len(keys) > 0 {
						break
					}
				}
				screen.Invalidate()
				render()
			}
		}
	}
}

// handleInspectorEvent dispatches one decoded event while the inspector screen
// is active. It returns true when the app should quit (Ctrl-C/Ctrl-D). Back
// commands close the inspector; refresh/follow touch the hub or viewport;
// scrolling keys and the wheel mutate the view and repaint. hubPtr is the loop's
// inspectorHub variable so a Refresh reaches the live hub.
func handleInspectorEvent(ev inputEvent, state *tuiState, hubPtr **InspectorHub, closeInspector, render func()) (quit bool) {
	if ev.kind == eventMouse {
		switch state.handleInspectorMouse(ev.mouse) {
		case commandBack:
			closeInspector()
		case commandRefreshInspector:
			if *hubPtr != nil {
				(*hubPtr).Refresh()
			}
		case commandFollowInspector:
			state.inspector.followBottom()
			render()
		case commandRender:
			render()
		}
		return false
	}

	switch inspectorKeyCommand(ev.key) {
	case commandQuit:
		return true
	case commandBack:
		closeInspector()
		return false
	}

	switch state.handleInspectorKey(ev.key) {
	case commandBack:
		closeInspector()
	case commandRefreshInspector:
		if *hubPtr != nil {
			(*hubPtr).Refresh()
		}
	case commandFollowInspector:
		state.inspector.followBottom()
		render()
	case commandRender:
		render()
	}
	return false
}

// sortRemotes returns a copy of the hub snapshot with each section's sessions
// sorted per mode. The snapshot's Session slices are shared with the hub
// goroutine, so the sort runs on fresh copies to avoid a data race.
// sortModeOrder is the 's'-key cycle; shift-s walks it backward.
var sortModeOrder = []string{"dir", "status", "created", "created-asc", "updated", "updated-asc"}

// cycleSortMode returns the mode delta steps away in sortModeOrder, wrapping
// at both ends. An unknown mode is treated as "dir" (index 0).
func cycleSortMode(mode string, delta int) string {
	i := 0
	for j, m := range sortModeOrder {
		if m == mode {
			i = j
			break
		}
	}
	n := len(sortModeOrder)
	return sortModeOrder[((i+delta)%n+n)%n]
}

// sortDesc is the human-readable label shown in the toast after cycling the
// sort mode with 's'.
func sortDesc(mode string) string {
	switch mode {
	case "status":
		return "status (waiting → idle → busy)"
	case "created":
		return "created ▼ (newest first)"
	case "created-asc":
		return "created ▲ (oldest first)"
	case "updated":
		return "updated ▼ (recently active first)"
	case "updated-asc":
		return "updated ▲ (least recently active first)"
	default:
		return "dir ▲ (cwd a→z)"
	}
}

func sortRemotes(remotes []RemoteResult, mode string) []RemoteResult {
	out := make([]RemoteResult, len(remotes))
	for i, r := range remotes {
		sorted := append([]Session(nil), r.Sessions...)
		SortSessions(sorted, mode)
		r.Sessions = sorted
		out[i] = r
	}
	return out
}

func sessionFooter() string {
	return dim("d disable/enable  ·  ? help")
}

func sessionBottomRow(toast string, toastActive bool) string {
	if toastActive {
		return bold(toast)
	}
	return sessionFooter()
}

// renderHelp builds help-screen content. RunTUI owns terminal positioning and
// sends this content through screenRenderer.
func renderHelp(sortMode string) string {
	var b strings.Builder
	fmt.Fprintln(&b, bold("claude-sessions  ·  help"))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("NAVIGATION"))
	fmt.Fprintln(&b, "    ↑ / ↓        move selection")
	fmt.Fprintln(&b, "    mouse click  select row · double-click opens")
	fmt.Fprintln(&b, "    mouse wheel  scroll list or inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("ACTIONS")+"  (on selected row)")
	fmt.Fprintln(&b, "    n            new tmux session (↑/↓ cwd · ←/→ command)")
	fmt.Fprintln(&b, "    d            disable / enable session")
	fmt.Fprintln(&b, "    k            kill the session (tmux-aware)")
	fmt.Fprintln(&b, "    a            attach (or migrate to tmux first)")
	fmt.Fprintln(&b, "    Enter / p    open full-screen inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("INSPECTOR"))
	fmt.Fprintln(&b, "    Home / End   oldest output / resume live follow")
	fmt.Fprintln(&b, "    PgUp / PgDn  scroll inspector by page")
	fmt.Fprintln(&b, "    r            refresh now")
	fmt.Fprintln(&b, "    Esc / q / p  return from inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("VIEW"))
	fmt.Fprintln(&b, "    m            cycle mode (full → intermediate → minimal)  ·  persisted")
	fmt.Fprintln(&b, "    s / S        cycle sort forward / back (dir → status → created → updated, +asc)")
	fmt.Fprintln(&b, "                 current sort: "+sortMode)
	fmt.Fprintln(&b, "    r            refresh now")
	fmt.Fprintln(&b, "    q / Ctrl-C   quit")
	fmt.Fprintln(&b, "    ?            this help")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("SUBCOMMANDS")+"  (from the shell)")
	fmt.Fprintln(&b, "    claude-sessions kill PID [-y]")
	fmt.Fprintln(&b, "    claude-sessions migrate PID [-y]")
	fmt.Fprintln(&b, "    claude-sessions new --cwd PATH [--name NAME]")
	fmt.Fprintln(&b, "    claude-sessions preview PID")
	fmt.Fprintln(&b, "    claude-sessions tmux-info PID")
	fmt.Fprintln(&b, "    claude-sessions attach PID")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, dim("press any key to return"))
	return b.String()
}
