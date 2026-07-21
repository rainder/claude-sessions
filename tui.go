package main

import (
	"fmt"
	"os"
	"strings"
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

// Key constants returned by parseEvents.
const (
	KeyUp    = "\x00up"
	KeyDown  = "\x00down"
	KeyLeft  = "\x00left"
	KeyRight = "\x00right"
	KeyEsc   = "\x00esc"
)

// parseEvents extracts keystrokes from a raw input chunk read from a terminal
// in raw mode. ESC sequences for arrow keys are recognized as KeyUp / KeyDown /
// KeyLeft / KeyRight; bare ESC becomes KeyEsc; everything else is the literal
// byte as a 1-rune string.
func parseEvents(buf []byte) []string {
	var out []string
	for len(buf) > 0 {
		if buf[0] == 0x1b {
			if len(buf) >= 3 && (buf[1] == '[' || buf[1] == 'O') {
				switch buf[2] {
				case 'A':
					out = append(out, KeyUp)
				case 'B':
					out = append(out, KeyDown)
				case 'C':
					out = append(out, KeyRight)
				case 'D':
					out = append(out, KeyLeft)
				default:
					out = append(out, KeyEsc)
				}
				buf = buf[3:]
				continue
			}
			out = append(out, KeyEsc)
			buf = buf[1:]
			continue
		}
		out = append(out, string(buf[0]))
		buf = buf[1:]
	}
	return out
}

// readEvents waits up to timeout for stdin (or the optional wakeFd) to become
// readable, then reads once and parses keystrokes. Returns nil keys on
// timeout or read error. When wakeFd >= 0 and it becomes readable, the pipe
// is drained and woke=true is returned — the caller should re-render to
// reflect the updated background state.
//
// We use unix.Select to wait rather than os.Stdin.SetReadDeadline because
// stdin inherited at process start isn't registered with Go's netpoller —
// SetReadDeadline silently no-ops there and Read blocks forever, which
// broke wall-clock ticking. Single consumer: no goroutine, so cooked-mode
// prompts (bufio.Scanner) and raw-mode polling never race.
func readEvents(timeout time.Duration, wakeFd int) (keys []string, woke bool) {
	fd := int(os.Stdin.Fd())
	maxFd := fd
	var fdSet unix.FdSet
	fdSet.Set(fd)
	if wakeFd >= 0 {
		fdSet.Set(wakeFd)
		if wakeFd > maxFd {
			maxFd = wakeFd
		}
	}
	var tvp *unix.Timeval
	if timeout > 0 {
		tv := unix.NsecToTimeval(timeout.Nanoseconds())
		tvp = &tv
	}
	n, err := unix.Select(maxFd+1, &fdSet, nil, nil, tvp)
	if err != nil || n == 0 {
		return nil, false
	}
	if wakeFd >= 0 && fdSet.IsSet(wakeFd) {
		var drain [64]byte
		for {
			if _, err := unix.Read(wakeFd, drain[:]); err != nil {
				break
			}
		}
		woke = true
	}
	if fdSet.IsSet(fd) {
		buf := make([]byte, 16)
		nr, err := unix.Read(fd, buf)
		if err == nil && nr > 0 {
			keys = parseEvents(buf[:nr])
		}
	}
	return keys, woke
}

// readEventBlocking waits indefinitely for the next event(s) from stdin.
// Useful inside action handlers that pause for input ("any key to continue").
// Background wakes are intentionally ignored so modal screens (e.g. help)
// aren't dismissed by remote-data updates underneath.
func readEventBlocking() []string {
	keys, _ := readEvents(0, -1)
	return keys
}

// RunTUI is the live view: alt-screen, raw mode, render-loop, key handler.
// Returns nil on clean quit (q / Ctrl-C / Ctrl-D), or an error if setup failed.
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

	// Alt-screen, hide cursor, disable line-wrap. Restored on return.
	fmt.Print("\033[?1049h\033[?25l\033[?7l")
	defer fmt.Print("\033[?7h\033[?25h\033[?1049l")

	viewMode := LoadViewMode()
	sortMode := LoadSortMode()
	var local []Session
	var remotes []RemoteResult
	var targets []selectionTarget
	sel := ""

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

	// refresh re-reads local sessions and the latest remote snapshot. When
	// kickRemote is true, the hub is also asked to refetch ASAP (used after
	// actions and the 'r' key). Wall-clock ticks pass false because the hub
	// has its own ticker — kicking on every tick would just double-fetch.
	refresh := func(kickRemote bool) {
		if s, err := CollectLocal(); err == nil {
			local = s
		}
		SortSessions(local, sortMode)
		if kickRemote {
			hub.Refresh()
		}
		// Snapshot() returns the hub's shared slices; sort remotes on copies so
		// we never race the hub goroutine that owns them.
		remotes = sortRemotes(hub.Snapshot(), sortMode)
		targets = buildSelectionTargets(local, remotes)
		sel = validateTargetSel(targets, sel)
	}
	// Render into a buffer and clip every line to the terminal width before
	// writing. Wrap is disabled (?7l), and with autowrap off a too-long line
	// makes each overflow char overwrite the last column — the row then ends
	// with the line's final char (e.g. the "s"/"m" of the AGE column) instead
	// of being cleanly cut.
	// Marquee animation is disabled for now: step stays 0, so an overflowing
	// DIR cell shows its static trimmed prefix. To re-enable, track step and
	// RenderAll's overflowing result here, cap the Select timeout below at
	// marqueeInterval while overflowing, and advance step on those expiries.
	//
	// toast is a transient one-liner (the sort mode after pressing 's')
	// pinned to the terminal's bottom row until toastUntil; the main loop
	// caps its wait at the deadline so the line vanishes on time.
	var toast string
	var toastUntil time.Time
	render := func() {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		var buf strings.Builder
		RenderAll(&buf, viewMode, local, remotes, sel, usageHub.Snapshot(), cols, 0, sortMode)
		out := clipLines(buf.String(), cols)
		if rows > 0 && time.Now().Before(toastUntil) {
			out += fmt.Sprintf("\033[%d;1H%s", rows, clipLine(bold(toast), cols))
		}
		fmt.Print("\033[H\033[J" + out)
	}

	makeCtx := func() *actCtx {
		return &actCtx{
			fd:       fd,
			oldState: oldState,
			targets:  targets,
			sel:      sel,
			pause:    func() { hub.Pause(); usageHub.Pause() },
			resume:   func() { hub.Resume(); usageHub.Resume() },
		}
	}

	refresh(false)
	render()

	// Wall-clock auto-refresh: tick every `interval` regardless of input.
	// readEvents takes the time remaining until the next tick; if it returns
	// empty, the tick fired and we refresh + advance. Otherwise we handle
	// keys and loop back without rescheduling.
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
		events, woke := readEvents(timeout, hub.WakeFD())
		if len(events) == 0 {
			// A toast deadline expired before the wall clock and without a
			// remote wake: repaint only (render drops the expired toast).
			if toastTick && !woke && time.Now().Before(nextTick) {
				render()
				continue
			}
			// Either the wall-clock tick fired (woke=false) or a remote-data
			// update landed (woke=true). Both paths refresh locals and
			// re-render, so a wake also resets the wall-clock tick — otherwise
			// the hub ticker and this tick double-render every cycle, drifting
			// past each other.
			refresh(false)
			render()
			nextTick = time.Now().Add(interval)
			continue
		}
		if woke {
			// Stdin and wake fired together. Refresh once so the key
			// handlers see the latest snapshot (e.g. nav uses fresh list).
			refresh(false)
		}
		for _, k := range events {
			switch k {
			case "q", "Q", "\x03", "\x04":
				return nil
			case KeyUp:
				sel = navTargets(targets, sel, -1)
				render()
			case KeyDown:
				sel = navTargets(targets, sel, 1)
				render()
			case "k", "K":
				actKill(makeCtx())
				refresh(true)
				render()
			case "a", "A":
				actAttach(makeCtx())
				refresh(true)
				render()
			case "p", "P":
				actPreview(makeCtx(), interval)
				refresh(true)
				render()
			case "n", "N":
				actNew(makeCtx())
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
				refresh(true)
				render()
			case "?":
				renderHelp(sortMode)
				readEventBlocking()
				render()
			}
		}
	}
}

// sortRemotes returns a copy of the hub snapshot with each section's sessions
// sorted per mode. The snapshot's Session slices are shared with the hub
// goroutine, so the sort runs on fresh copies to avoid a data race.
// sortModeOrder is the 's'-key cycle; shift-s walks it backward.
var sortModeOrder = []string{"dir", "created", "created-asc", "updated", "updated-asc"}

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

// renderHelp paints the help modal. Caller waits for a keypress to dismiss.
func renderHelp(sortMode string) {
	fmt.Print("\033[H\033[J")
	fmt.Println(bold("claude-sessions  ·  help"))
	fmt.Println()
	fmt.Println("  " + bold("NAVIGATION"))
	fmt.Println("    ↑ / ↓        move selection")
	fmt.Println()
	fmt.Println("  " + bold("ACTIONS") + "  (on selected row)")
	fmt.Println("    n            new tmux session (↑/↓ cwd · ←/→ command)")
	fmt.Println("    k            kill the session (tmux-aware)")
	fmt.Println("    a            attach (or migrate to tmux first)")
	fmt.Println("    p            preview (tmux pane snapshot or transcript tail)")
	fmt.Println()
	fmt.Println("  " + bold("VIEW"))
	fmt.Println("    m            cycle mode (full → intermediate → minimal)  ·  persisted")
	fmt.Println("    s / S        cycle sort forward / back (dir → created → updated, +asc)")
	fmt.Println("                 current sort: " + sortMode)
	fmt.Println("    r            refresh now")
	fmt.Println("    q / Ctrl-C   quit")
	fmt.Println("    ?            this help")
	fmt.Println()
	fmt.Println("  " + bold("SUBCOMMANDS") + "  (from the shell)")
	fmt.Println("    claude-sessions kill PID [-y]")
	fmt.Println("    claude-sessions migrate PID [-y]")
	fmt.Println("    claude-sessions new --cwd PATH [--name NAME]")
	fmt.Println("    claude-sessions preview PID")
	fmt.Println("    claude-sessions tmux-info PID")
	fmt.Println("    claude-sessions attach PID")
	fmt.Println()
	fmt.Println(dim("press any key to return"))
}
