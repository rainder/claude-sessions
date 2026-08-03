package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

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

// inspectorChromeRows is the number of *fixed* rows RenderInspector reserves
// around the scrolling body (title, metadata, separator, footer). A loaded
// ticket summary costs inspectorSummaryExtraRows on top of it, so the viewport
// height is the terminal height minus both, and must match the body arithmetic
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
	// store holds client-side group assignments only (state.go); disabled
	// state lives in disabledStore (disabled_store.go), host-owned and
	// persisted separately. groupFilterState (zero value = no filter) is
	// runtime-only — never persisted. pendingHide arms the inverse-filter
	// binding: 'h' sets it and the next keystroke resolves it (see
	// groupFilterTransition). textFilter is the '/'-driven free-text filter
	// (also runtime-only); its effective query composes with groupFilterState
	// (AND). groups is a per-settle snapshot of the store's assignments,
	// reused for badge rendering and the filter predicate. hideDisabled is the
	// 'd' toggle (also runtime-only) that composes (AND) with the group
	// filter and text query.
	store := LoadSessionStore()
	disabledStore := LoadDisabledStore()
	var groupFilterState groupFilter
	var pendingHide bool
	var textFilter textFilterState
	var hideDisabled bool
	var groups map[string]int
	var lastStateTouch time.Time
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

	// Account usage bars: same non-blocking pattern as the remote hub. The first
	// paint happens with no bar; it appears once the initial fetch lands (no wake
	// pipe — the next tick repaints anyway). Both snapshots arrive account-paired
	// at fetch time (the Anthropic email is re-read each fetch, so a mid-run
	// relogin re-attributes the bar; the Codex email rides in its payload), so
	// each bar can be deduped/labeled against remotes on a different account.
	usageHub := NewUsageHub()
	defer usageHub.Shutdown()

	codexUsageHub := NewCodexUsageHub()
	defer codexUsageHub.Shutdown()

	// Every account this machine holds a claude-switch credential snapshot for,
	// polled the same way but read-only from the snapshot files — the live
	// credential is never touched, so the account actually logged in here keeps
	// working exactly as before.
	knownAccountsHub := NewKnownAccountsHub()
	defer knownAccountsHub.Shutdown()

	// Each remote host's accounts come from its own /usage endpoint rather than
	// riding /sessions, so a host nobody is watching never calls Anthropic at
	// all. Each tick tells every host which accounts THIS machine already has
	// good numbers for, so a remote host skips fetching an account this client
	// is already showing from its own live poll — the redundant case this whole
	// design exists to cut. It does not dedupe across remotes with no bearing on
	// this client: two remotes sharing an account neither client account covers
	// still each fetch it independently.
	remoteUsageHub := NewRemoteUsageHub(func() []string {
		return localFreshAccountEmails(
			usageHub.Snapshot(),
			derefKnownAccounts(knownAccountsHub.Snapshot()),
		)
	})
	defer remoteUsageHub.Shutdown()

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

	// Remember the size this terminal runs at, so a detached tmux session created
	// later by something *without* a terminal — the headless server's spawn,
	// migrate and resume handlers — has a better guess than tmux's 80x24 default.
	// Offered every frame; the recorder debounces and drops non-positive values.
	sizeRecorder := newTUISizeRecorder()

	// inspectorHub polls the previewed session while the inspector screen is
	// open; nil on the session list. Shut down on exit if still open.
	var inspectorHub *InspectorHub
	// ticketSummarySec fetches the DR-XXXX summary for the inspected session, the
	// same cache-backed fetch the 'i' info dialog runs; nil when the open session
	// carries no ticket id (and on the session list). Its result is drawn above
	// the inspector body only once it has landed — nothing is reserved for it
	// while it is still in flight.
	var ticketSummarySec *asyncSection

	// resizeInspected fires a best-effort tmux resize (or, with revert=true,
	// an un-pin) for sess, dispatching local vs. remote exactly like
	// sendKeysToInspected below. Errors are never surfaced: this is a display
	// enhancement, not something that can block entering or leaving preview —
	// preview content renders via capture-pane regardless of the pane's
	// current size (see docs/superpowers/specs/
	// 2026-08-03-preview-resize-design.md).
	//
	// A **revert** whose fresh resolve fails falls back to sess.Tmux, the pane
	// address the snapshot was built with. Without it, a session that exits
	// while it is being previewed takes its resolveLivePIDLocal entry with it,
	// the revert silently returns, and a hand-managed tmux window that outlives
	// the claude process stays pinned to window-size=manual forever — the one
	// failure mode this whole feature exists to avoid. A stale address here can
	// only un-pin a window that is already gone or has been reused, which is
	// why the same fallback would be wrong for send-keys (it types text) and is
	// deliberately not taken on the **entry** resize: a wrong pane resized is a
	// fresh pin on a stranger's window, and there is no stuck state for an
	// entry resize to recover in the first place.
	resizeInspected := func(sess Session, cols, rows int, revert bool) {
		if sess.Host == "" {
			live, err := resolveLivePIDLocal(sess.PID, sess.SessionID)
			if err != nil {
				if revert {
					_ = revertTmuxTarget(sess)
				}
				return
			}
			_ = resizeSession(live, cols, rows, revert)
			return
		}
		_, _ = resizeRemote(sess.Host, sess.PID, sess.SessionID, cols, rows, revert)
	}

	defer func() {
		if inspectorHub != nil {
			// Covers every RunTUI return path that skips closeInspector — most
			// notably quitting outright (Ctrl+D/'q') while the inspector is
			// open, which previously left the target's tmux window pinned to
			// manual-size mode forever. A hard process kill (SIGKILL) still
			// bypasses this — accepted, see the design spec's known
			// limitations.
			resizeInspected(state.inspector.snapshot.Session, 0, 0, true)
			inspectorHub.Shutdown()
		}
		ticketSummarySec.close() // nil-safe
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
		// Refresh the group snapshot and overlay this host's disabled flags
		// onto local sessions before sorting — the sort orders disabled rows
		// last. Remote sessions already carry authoritative Disabled from the
		// wire (each remote host's own DisabledStore, applied server-side in
		// GET /sessions), so no client-side overlay is needed for them.
		groups = store.GroupsMap()
		disabledStore.Overlay(local)
		SortSessions(local, sortMode)
		// Snapshot() returns the hub's shared slices; sort remotes on copies so
		// we never race the hub goroutine that owns them.
		snap := hub.Snapshot()
		remotes = sortRemotes(snap, sortMode)
		// The account fields ride their own poll now, not /sessions, so overlay
		// them here — before anything reads `remotes`, whether that is the header's
		// rate-limit bars, a host heading's account label, or the Ctrl+W picker.
		remotes = overlayRemoteUsage(remotes, remoteUsageHub.Snapshot())
		// Keep long-lived group entries alive under plain viewing: refresh
		// their last_seen about hourly so the 30-day load-time GC never drops
		// state for a session that's still on screen. Disabled's own
		// keepalive is handled separately by DisabledStore.Overlay's
		// opportunistic touch above, not by this TouchVisible call.
		if now := time.Now(); now.Sub(lastStateTouch) >= time.Hour {
			lastStateTouch = now
			store.TouchVisible(visibleSessionIDs(local, remotes))
		}
		// Targets mirror exactly what the frame renders: filtered by the active
		// group and text query so a filtered-out selection falls back via
		// validateTargetSel.
		gv := groupView{groups: groups, filter: groupFilterState, query: textFilter.effectiveQuery(), hideDisabled: hideDisabled}
		targets = buildSelectionTargets(
			filterSessionRows(local, gv),
			filterRemoteResults(remotes, gv),
		)
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
		sizeRecorder.record(cols, rows)

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
			// One derivation feeds both the viewport size and the drawn block —
			// two would be free to drift, and the body arithmetic is what keeps
			// scrolling and the footer row in agreement.
			summaryLines := inspectorSummaryFit(inspectorTicketSummaryLines(ticketSummarySec.snapshot(), cols), rows)
			state.inspector.resize(rows - inspectorChromeRows - inspectorSummaryExtraRows(summaryLines))
			var buf strings.Builder
			state.hits = RenderInspector(&buf, state.inspector, summaryLines, cols, rows)
			_ = screen.Draw(buf.String(), cols, rows)
			return
		}

		frame := BuildTableFrame(viewMode, LocalHost{
			Name:      localName,
			Sessions:  local,
			HostUsage: hostUsageHub.Snapshot(),
		}, remotes, state.sel, &LocalUsage{
			Claude:        usageHub.Snapshot(),
			Codex:         codexUsageHub.Snapshot(),
			KnownAccounts: derefKnownAccounts(knownAccountsHub.Snapshot()),
		}, cols, 0, sortMode, groupView{groups: groups, filter: groupFilterState, query: textFilter.effectiveQuery(), hideDisabled: hideDisabled})
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
			// While '/'-input mode is active, the edit prompt replaces the footer
			// hint (and any toast) so the user sees the query they're typing.
			bottom := sessionBottomRow(toast, toastActive)
			if textFilter.editing {
				bottom = textFilterPrompt(textFilter.buffer, cols)
			}
			out = withBottomRow(out, rows, bottom)
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
			disabled:   disabledStore,
			// Straight from the pollers' latest snapshots (and, for a remote row,
			// the last /sessions poll this loop already has in `remotes`), so
			// Ctrl+W opens instantly and never fetches.
			accounts: func(host string) accountSnapshot {
				if host == "" {
					known := knownAccountsHub.Snapshot()
					return accountSnapshot{
						Usage:      usageHub.Snapshot(),
						Known:      derefKnownAccounts(known),
						ActiveName: activeSnapshotNameOf(known),
					}
				}
				for _, r := range remotes {
					if r.Name == host {
						return accountSnapshotOf(r)
					}
				}
				return accountSnapshot{}
			},
			pause: func() {
				hub.Pause()
				usageHub.Pause()
				codexUsageHub.Pause()
				knownAccountsHub.Pause()
				remoteUsageHub.Pause()
				hostUsageHub.Pause()
			},
			resume: func() {
				hub.Resume()
				usageHub.Resume()
				codexUsageHub.Resume()
				knownAccountsHub.Resume()
				remoteUsageHub.Resume()
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
		// Start the ticket fetch only once the hub is up: a failed NewInspectorHub
		// returns above with no inspector open and no closeInspector to reap it.
		// closeInspector always runs before the list can reopen, so there is
		// nothing live here — close anyway rather than rely on that invariant.
		ticketSummarySec.close() // nil-safe
		ticketSummarySec = nil
		if ticketID := detectTicketID(sess.CWD, sess.Name); ticketID != "" {
			ticketSummarySec = startAsyncSection("ticket", func(ctx context.Context) (PreviewResult, error) {
				return fetchTicketSummaryCached(ctx, ticketID)
			})
		}
		if cols, rows, err := term.GetSize(fd); err == nil && cols > 0 {
			// Best-effort only: the ticket-summary block hasn't loaded yet at
			// this point, so this slightly under-reserves rows on first open —
			// the accepted one-shot tradeoff documented in the design spec.
			// Synchronous, not fired in a goroutine: closeInspector's and the
			// quit-path defer's reverts assume the entry resize has already
			// landed, so an async entry can race a quick close/quit and leave
			// the window pinned to window-size=manual with no un-pin ever
			// following it. Bounded at 5s worst case by resizeRemote's own
			// timeout (remote_actions.go) — the same shape send_keys.go's
			// local resolveLivePIDLocal path already accepts on the UI thread.
			if innerRows := rows - inspectorChromeRows; innerRows > 0 {
				resizeInspected(sess, cols, innerRows, false)
			}
		}
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
			resizeInspected(state.inspector.snapshot.Session, 0, 0, true)
			inspectorHub.Shutdown()
			inspectorHub = nil
		}
		ticketSummarySec.close() // nil-safe; same reason the hub reference is dropped
		ticketSummarySec = nil
		state.mode = screenSessions
		state.inspector = inspectorViewState{}
		state.inspectorTargetGone = false
		refresh(false)
		screen.Invalidate()
		render()
	}

	// sendKeysToInspected sends text into the currently-inspected session's
	// tmux pane, dispatching local vs. remote exactly like every other action
	// in this loop (target.Host != ""). Local goes through a fresh
	// resolveLivePIDLocal (send_keys.go) so the pane address is current, not
	// whatever this Session snapshot last polled; remote goes through the
	// authed POST /sessions/{pid}/send-keys endpoint (server.go).
	sendKeysToInspected := func(sess Session, text string) (bool, string) {
		if sess.Host == "" {
			live, err := resolveLivePIDLocal(sess.PID, sess.SessionID)
			if err != nil {
				return false, err.Error()
			}
			if err := sendKeys(live, text); err != nil {
				return false, err.Error()
			}
			return true, ""
		}
		r, err := sendKeysRemote(sess.Host, sess.PID, sess.SessionID, text)
		if err != nil {
			return false, err.Error()
		}
		if !r.OK {
			return false, r.Error
		}
		return true, ""
	}

	refresh(false)
	render()

	// Wall-clock auto-refresh: tick every `interval` regardless of input.
	// pollEvents takes the time remaining until the next tick; if it returns
	// empty and unwoken, the tick fired and we refresh + advance.
	nextTick := time.Now().Add(interval)

	for {
		timeout := time.Until(nextTick)
		// While a toast (session-list screen) or a compose-send status
		// (inspector screen) is showing, wake at its deadline so the message
		// clears on time. toastTick marks a wait capped for that reason: its
		// expiry repaints only, leaving the wall-clock cadence untouched.
		statusUntil := toastUntil
		if state.mode == screenInspector {
			statusUntil = state.inspector.composeStatusUntil
		}
		toastTick := false
		if until := time.Until(statusUntil); until > 0 && until < timeout {
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
		// No nil guard: pane() and wake() are both nil-receiver-safe and wake()
		// reports fd -1 when there is nothing to wait on, which pollEvents skips.
		wakes = append(wakes, ticketSummarySec.pane().wake())
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
				// Resize, inspector update and/or a landed ticket summary
				// (wakePreview) only: redraw at the current size (render re-reads
				// it) without disturbing the wall clock.
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
				quit, absorb := handleInspectorEvent(ev, state, &inspectorHub, closeInspector, render, func() {
					screen.Invalidate()
					actAttach(makeCtx())
					refresh(true)
				}, func() {
					screen.Invalidate()
					actKill(makeCtx())
					refresh(true)
				}, sendKeysToInspected)
				if quit {
					return nil
				}
				if absorb {
					// A paste's trailing bytes after the newline that just
					// submitted (or cancelled) compose mode belong to the
					// compose box, not this screen's hotkeys — drop them
					// rather than let e.g. a trailing 'k' fall through to
					// kill. See handleInspectorEvent's doc comment.
					break
				}
				continue
			}
			if ev.kind == eventMouse {
				switch state.handleListMouse(ev.mouse, time.Now()) {
				case commandOpenInspector:
					// Any pending 'h' arm dies here: the inspector consumes keys of
					// its own, and a stale arm would flip the next digit into hide mode.
					pendingHide = false
					openInspector()
				case commandRender:
					render()
				}
				continue
			}
			k := ev.key
			// Free-text filter ('/') runs through a pure transition ahead of every
			// other key handler: while input mode is active it captures all keys
			// (hotkeys suspended); otherwise only '/' is consumed, entering input
			// mode preloaded with the committed query. Any consumed key re-filters
			// and re-renders. Entering input mode also cancels a pending 'h' arm so a
			// stale arm can't leak into the next digit.
			if next, consumed := textFilterTransition(textFilter, k); consumed {
				if next.editing && !textFilter.editing {
					pendingHide = false
				}
				textFilter = next
				settleRows()
				state.requestSelectionAnchor()
				render()
				continue
			}
			if sessionKeyCommand(k) == commandOpenInspector {
				pendingHide = false
				openInspector()
				continue
			}
			// Group view filter (digits, the 'h' arm, and armed hide toggles) runs
			// through a pure transition so the state machine stays table-testable. A
			// consumed key updates the filter and never falls through; a non-filter
			// key reports consumed=false with the arm cleared, so it drops into the
			// normal switch below (which is how kill/quit still fire while armed).
			if nextFilter, nextArmed, consumed := groupFilterTransition(groupFilterState, pendingHide, k); consumed {
				pendingHide = nextArmed
				if nextFilter != groupFilterState {
					groupFilterState = nextFilter
					settleRows()
					state.requestSelectionAnchor()
					render()
				}
				continue
			} else {
				pendingHide = nextArmed
			}
			switch k {
			case "q", "Q", "\x03", "\x04":
				return nil
			case KeyEnter:
				screen.Invalidate()
				actAttach(makeCtx())
				refresh(true)
				render()
			case KeyUp:
				state.navigate(targets, -1)
				render()
			case KeyDown:
				state.navigate(targets, 1)
				render()
			case "\t":
				state.selectID(firstIdleTarget(targets))
				render()
			case "\x0b": // Ctrl+K
				screen.Invalidate()
				actKill(makeCtx())
				refresh(true)
				render()
			case "i", "I":
				screen.Invalidate()
				actInfo(makeCtx())
				screen.Invalidate()
				render()
			case "a", "A":
				screen.Invalidate()
				actAttach(makeCtx())
				refresh(true)
				render()
			case "-", "+":
				screen.Invalidate()
				if actToggleDisabled(makeCtx()) {
					// The store write is authoritative; refresh(true) re-overlays
					// it onto local (via settleRows) and kicks an immediate remote
					// refetch so a remote toggle shows up promptly — the same
					// convention actKill/actAttach already follow, including the
					// same brief eventual-consistency window if a poll was already
					// in flight when the write landed.
					refresh(true)
					state.requestSelectionAnchor()
					render()
				}
			case "\x17": // Ctrl+W: switch the selected row's host to another account
				screen.Invalidate()
				msg, switched := actSwitchAccount(makeCtx())
				if switched {
					// The local account pollers still describe the *previous*
					// account for up to two minutes; kick both so this machine's
					// header email and picker marker catch up with the toast.
					usageHub.Kick()
					knownAccountsHub.Kick()
					// A remote switch needs no kick to be *correct* — every host
					// re-resolves which account is live on each /usage call — but
					// its percentages are cached per account, so refetch so the
					// bars follow the switch instead of lagging a tick behind.
					remoteUsageHub.Refresh()
					refresh(true)
				}
				if msg != "" {
					toast = msg
					toastUntil = time.Now().Add(4 * time.Second)
				}
				render()
			case "d", "D":
				hideDisabled = !hideDisabled
				settleRows()
				state.requestSelectionAnchor()
				render()
			case "!", "@", "#", "$", "%", "^", "&", "*", "(":
				// Shift+1..9 assign the selected session's group (single membership;
				// same group again ungroups). Sessions with no SessionID are ignored.
				if group := shiftDigitGroup(k); group != 0 {
					if s := findSelectionTarget(targets, state.sel); s != nil && s.session != nil && s.session.SessionID != "" {
						store.SetGroup(s.session.SessionID, group, visibleSessionIDs(local, remotes))
						settleRows()
						state.requestSelectionAnchor()
						render()
					}
				}
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
				if ctx.spawnedBackground {
					toast = "spawned " + ctx.spawnedTmux + " in background"
					toastUntil = time.Now().Add(4 * time.Second)
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
				screen.Invalidate()
				if picked, ok := pickSortMode(sortMode, modalWakes); ok && picked != sortMode {
					sortMode = picked
					SaveSortMode(sortMode)
					toast = "sort: " + sortDesc(sortMode)
					toastUntil = time.Now().Add(4 * time.Second)
					refresh(false)
				}
				screen.Invalidate()
				render()
			case "r", "R":
				screen.Invalidate()
				ctx := makeCtx()
				actResume(ctx)
				// Chase the resumed session's tmux pane across refreshes, same as
				// the new-session landing (only a real spawn sets spawnedTmux).
				if ctx.spawnedTmux != "" {
					state.pending = &pendingSpawn{host: ctx.spawnedHost, tmux: ctx.spawnedTmux}
				}
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
// is active. It returns quit=true when the app should exit (Ctrl-C/Ctrl-D
// outside compose mode, or Ctrl-D while composing — composing itself
// intercepts a bare Ctrl-C as cancel, not quit). absorbBatch is true exactly
// when this event ended compose mode (a successful submit or a cancel): the
// caller must then drop any remaining events already queued from the same
// input read (i.e. the rest of the current Feed()/decode batch — not
// necessarily the rest of a paste: a remote send can block on sendText for up
// to remoteRequest's ~30s timeout, and any key typed during that wait arrives
// in a later, separate read after compose mode has already ended, so it is
// NOT covered by this and dispatches as a normal hotkey instead of being
// discarded). Without this, a paste containing an embedded newline mid-buffer
// would submit on that newline and let its trailing bytes fall through to
// this screen's normal hotkeys (e.g. Ctrl+K triggering kill) instead of being
// discarded along with the rest of the batch. Back commands close the
// inspector; refresh/follow touch the hub or viewport; scrolling keys and the
// wheel mutate the view and repaint. hubPtr is the loop's inspectorHub

// variable so a Refresh reaches the live hub. Enter attaches to the session
// (mirroring the session-list Enter binding) and closes the inspector — but
// only outside compose mode, where Enter instead submits the compose buffer.
// Ctrl+K opens the kill confirmation (mirroring the session-list Ctrl+K
// binding) and closes the inspector. 'i' and a click on the footer's Compose
// control both arm compose mode via state.armCompose(), which no-ops with a
// dim "no tmux pane" hint when the session has no pane to send into. sendText
// performs the actual send (local vs. remote is the caller's concern, not
// this function's) and is only ever invoked with a non-empty compose buffer.
func handleInspectorEvent(ev inputEvent, state *tuiState, hubPtr **InspectorHub, closeInspector, render, attach, kill func(), sendText func(Session, string) (bool, string)) (quit, absorbBatch bool) {
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
		case commandComposeInspector:
			state.armCompose()
			render()
		case commandRender:
			render()
		}
		return false, false
	}

	if state.inspector.composing {
		if ev.key == "\x04" {
			return true, false
		}
		submit, cancel := state.handleInspectorCompose(ev.key)
		switch {
		case cancel:
			render()
			return false, true
		case submit:
			text := state.inspector.composeText
			sess := state.inspector.snapshot.Session
			state.inspector.composeStatus = "sending…"
			render()
			ok, msg := sendText(sess, text)
			state.inspector.composeStatusUntil = time.Now().Add(4 * time.Second)
			if ok {
				state.inspector.composing = false
				state.inspector.composeText = ""
				state.inspector.composeStatus = "sent"
			} else {
				state.inspector.composeStatus = msg
			}
			render()
			return false, !state.inspector.composing
		default:
			render()
			return false, false
		}
	}

	if ev.key == KeyEnter {
		closeInspector()
		attach()
		return false, false
	}

	if ev.key == "i" {
		state.armCompose()
		render()
		return false, false
	}

	switch inspectorKeyCommand(ev.key) {
	case commandQuit:
		return true, false
	case commandBack:
		closeInspector()
		return false, false
	}

	switch state.handleInspectorKey(ev.key) {
	case commandBack:
		closeInspector()
	case commandKillInspector:
		kill()
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
	return false, false
}

// sortRemotes returns a copy of the hub snapshot with each section's sessions
// sorted per mode. The snapshot's Session slices are shared with the hub
// goroutine, so the sort runs on fresh copies to avoid a data race.
// sortModeOrder is the row order of the 's' sort picker (sort_picker.go).
var sortModeOrder = []string{"dir", "status", "created", "created-asc", "updated", "updated-asc"}

// sortDesc is the human-readable label for a sort mode: the toast after a
// change, and every row of the sort picker — where its width also sizes the
// dialog box.
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

// groupFilterMode selects how a groupFilter's mask is read: no filter, show
// only the masked groups, or hide the masked groups. filterNone must stay the
// zero value so a bare groupFilter{} (and groupView{}) means "show everything".
type groupFilterMode uint8

const (
	filterNone groupFilterMode = iota
	filterOnly
	filterHide
)

// groupFilter is the runtime-only view filter (never persisted). filterOnly
// makes a session visible iff its group's bit is set in mask (a single bit
// today; ungrouped rows are hidden). filterHide makes a session visible iff it
// is ungrouped or its bit is NOT set (ungrouped rows always survive). Bits
// 1..9 map to groups 1..9.
type groupFilter struct {
	mode groupFilterMode
	mask uint16
}

// groupMaskHas reports whether group's bit (1..9) is set in mask.
func groupMaskHas(mask uint16, group int) bool {
	if group < 1 || group > 9 {
		return false
	}
	return mask&(1<<uint(group)) != 0
}

// isDigit1to9 reports whether k is a single '1'..'9' keystroke.
func isDigit1to9(k string) bool {
	return len(k) == 1 && k[0] >= '1' && k[0] <= '9'
}

// groupFilterTransition computes the next filter state after key k, given the
// current filter and whether an 'h' arm is pending. consumed reports whether k
// was a filter binding; when false the caller cancels any arm (nextArmed is
// always false then) and processes k through the normal key switch. Keeping it
// a pure function makes the digit / hide / arm state machine table-testable.
//
// Bindings: 1..9 select only-mode for that single group (same digit again, or
// 0, clears; pressing while hide is active switches to only); h/H arms a
// pending state whose next 1..9 toggles that group's bit in hide mode (removing
// the last bit clears; switching from only starts a fresh mask). An armed
// non-digit cancels the arm and is reinterpreted unarmed, so 'h' re-arms and
// any other key falls through.
func groupFilterTransition(cur groupFilter, armed bool, k string) (next groupFilter, nextArmed, consumed bool) {
	if armed && isDigit1to9(k) {
		n := int(k[0] - '0')
		mask := uint16(0)
		if cur.mode == filterHide {
			mask = cur.mask
		}
		mask ^= 1 << uint(n)
		if mask == 0 {
			return groupFilter{}, false, true
		}
		return groupFilter{mode: filterHide, mask: mask}, false, true
	}
	// Not an armed digit: any pending arm is cancelled, then k is read as an
	// unarmed keystroke.
	if k == "0" {
		return groupFilter{}, false, true
	}
	if isDigit1to9(k) {
		n := int(k[0] - '0')
		bit := uint16(1) << uint(n)
		if cur.mode == filterOnly && cur.mask == bit {
			return groupFilter{}, false, true // same digit again clears
		}
		return groupFilter{mode: filterOnly, mask: bit}, false, true
	}
	if k == "h" || k == "H" {
		return cur, true, true // arm; the filter is unchanged until the next key
	}
	return cur, false, false
}

// shiftDigitGroup maps a US-layout Shift+1..9 keystroke to its group number
// (1..9), or 0 for any other key.
func shiftDigitGroup(key string) int {
	const shifted = "!@#$%^&*("
	if len(key) != 1 {
		return 0
	}
	if i := strings.IndexByte(shifted, key[0]); i >= 0 {
		return i + 1
	}
	return 0
}

// textFilterState is the runtime-only free-text filter driven by '/': a
// committed query that narrows the visible rows, plus a transient buffer holding
// the query being typed while input mode is active. Never persisted.
type textFilterState struct {
	editing   bool   // true while '/'-input mode is active
	buffer    string // the query being edited (meaningful only while editing)
	committed string // the active committed query ("" = no text filter)
}

// effectiveQuery is the query currently narrowing the rows: the live edit buffer
// while editing (so rows narrow incrementally as the user types), otherwise the
// committed query.
func (t textFilterState) effectiveQuery() string {
	if t.editing {
		return t.buffer
	}
	return t.committed
}

// textFilterTransition applies key k to the text-filter state and reports
// whether it was consumed. When not editing, only '/' is consumed: it enters
// input mode preloaded with the committed query for editing; every other key
// reports consumed=false so the caller runs its normal key handling. While
// editing, every key is consumed (all hotkeys are suspended) except Ctrl+C /
// Ctrl+D, which pass through so the hard-quit stays live: text input (ASCII
// printable, and multi-byte input appended as-is) extends the buffer, Backspace
// (DEL/BS) deletes the last rune, Ctrl+U clears the buffer, Enter commits it
// (an empty buffer clears the filter), and Esc cancels and clears the query.
// Any other key (a suspended hotkey or navigation key) is swallowed unchanged.
// Pure so the state machine is table-testable.
func textFilterTransition(cur textFilterState, k string) (next textFilterState, consumed bool) {
	if !cur.editing {
		if k == "/" {
			return textFilterState{editing: true, buffer: cur.committed, committed: cur.committed}, true
		}
		return cur, false
	}
	switch {
	case k == "\x03" || k == "\x04":
		// The universal hard-quit stays live mid-edit: report not-consumed so the
		// caller's normal key switch handles Ctrl+C / Ctrl+D.
		return cur, false
	case k == KeyEnter:
		return textFilterState{committed: cur.buffer}, true
	case k == KeyEsc:
		return textFilterState{}, true
	case k == "\x7f" || k == "\x08": // DEL / Backspace
		return textFilterState{editing: true, buffer: trimLastRune(cur.buffer), committed: cur.committed}, true
	case k == "\x15": // Ctrl+U clears the line
		return textFilterState{editing: true, buffer: "", committed: cur.committed}, true
	case isTextInput(k):
		return textFilterState{editing: true, buffer: cur.buffer + k, committed: cur.committed}, true
	default:
		return cur, true
	}
}

// isTextInput reports whether decoded key k is literal text to append to the
// filter buffer rather than a control or navigation key. Control bytes (< 0x20)
// and DEL (0x7f) are rejected, which also excludes the "\x00…" navigation
// sentinels (KeyUp, KeyEnter, KeyEsc, …); printable ASCII and multi-byte input
// (which the decoder emits as high bytes) pass.
func isTextInput(k string) bool {
	if k == "" {
		return false
	}
	b := k[0]
	return b >= 0x20 && b != 0x7f
}

// trimLastRune drops the final UTF-8 rune of s (a no-op on the empty string), so
// Backspace deletes a whole rune rather than a stray byte.
func trimLastRune(s string) string {
	if s == "" {
		return s
	}
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}

// textFilterPrompt is the bottom row shown while '/'-input mode is active: the
// query buffer after a leading '/', capped by a block cursor. It replaces the
// footer hint line so the user sees exactly what they are typing. A buffer too
// long for the terminal keeps its tail (where the cursor is), dropping leading
// runes, so typing never pushes the cursor off-screen.
func textFilterPrompt(buffer string, cols int) string {
	if cols > 2 { // leading '/' + cursor block
		if runes := []rune(buffer); len(runes) > cols-2 {
			buffer = string(runes[len(runes)-(cols-2):])
		}
	}
	return "/" + buffer + "▌"
}

// visibleSessionIDs collects the SessionIDs of every session currently in view
// (local + all remotes), for refreshing the store's last_seen on save. Blank
// IDs are skipped.
func visibleSessionIDs(local []Session, remotes []RemoteResult) []string {
	ids := make([]string, 0, len(local))
	for _, s := range local {
		if s.SessionID != "" {
			ids = append(ids, s.SessionID)
		}
	}
	for _, r := range remotes {
		for _, s := range r.Sessions {
			if s.SessionID != "" {
				ids = append(ids, s.SessionID)
			}
		}
	}
	return ids
}

func sessionFooter() string {
	return dim("-/+ disable/enable  ·  d hide disabled  ·  1-9 only  ·  h1-9 hide  ·  ⇧1-9 group  ·  / search  ·  ? help")
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
	fmt.Fprintln(&b, "    Tab          jump to topmost idle (or shell) session")
	fmt.Fprintln(&b, "    mouse click  select row · double-click opens")
	fmt.Fprintln(&b, "    mouse wheel  scroll list or inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("ACTIONS")+"  (on selected row)")
	fmt.Fprintln(&b, "    n            new tmux session (↑/↓ cwd · ←/→ command · p prompt in background)")
	fmt.Fprintln(&b, "    r            resume a past session (searchable · local + remote)")
	fmt.Fprintln(&b, "    - / +        disable / enable session")
	fmt.Fprintln(&b, "    Shift-1..9   assign session to group ①..⑨ (same group again ungroups)")
	fmt.Fprintln(&b, "    Ctrl-K       kill the session (tmux-aware)")
	fmt.Fprintln(&b, "    a            attach (or migrate to tmux first)")
	fmt.Fprintln(&b, "    Enter / p    open full-screen inspector")
	fmt.Fprintln(&b, "    i            show session info (ticket + conversation summary)")
	fmt.Fprintln(&b, "    Ctrl-W       switch that host's Claude account (⏎ applies · esc cancels)")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("INSPECTOR"))
	fmt.Fprintln(&b, "    Home / End   oldest output / resume live follow")
	fmt.Fprintln(&b, "    PgUp / PgDn  scroll inspector by page")
	fmt.Fprintln(&b, "    r            refresh now")
	fmt.Fprintln(&b, "    Ctrl-K       kill the session (tmux-aware)")
	fmt.Fprintln(&b, "    Esc / q / p  return from inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("VIEW"))
	fmt.Fprintln(&b, "    m            cycle mode (full → intermediate → minimal)  ·  persisted")
	fmt.Fprintln(&b, "    1..9         show only group (same digit or 0 shows all)")
	fmt.Fprintln(&b, "    h then 1..9  hide group(s) (repeat to add/remove · last one shows all)")
	fmt.Fprintln(&b, "    d            hide/show disabled sessions")
	fmt.Fprintln(&b, "    /            filter rows by text (type to narrow · Enter commits · Esc clears)")
	fmt.Fprintln(&b, "    s / S        open sort-by dialog (↑/↓ select · ⏎ confirm · esc cancel)")
	fmt.Fprintln(&b, "                 current sort: "+sortMode)
	fmt.Fprintln(&b, "    q / Ctrl-C   quit")
	fmt.Fprintln(&b, "    ?            this help")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("SUBCOMMANDS")+"  (from the shell)")
	fmt.Fprintln(&b, "    claude-sessions kill PID [-y]")
	fmt.Fprintln(&b, "    claude-sessions migrate PID [-y]")
	fmt.Fprintln(&b, "    claude-sessions new --dir PATH [--name NAME] [--command PRESET] [--server SERVER] [PROMPT...]")
	fmt.Fprintln(&b, "    claude-sessions preview PID")
	fmt.Fprintln(&b, "    claude-sessions tmux-info PID")
	fmt.Fprintln(&b, "    claude-sessions attach PID")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, dim("press any key to return"))
	return b.String()
}
