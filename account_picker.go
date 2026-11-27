package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"golang.org/x/term"
)

// The Ctrl+W account picker: a small bordered overlay listing the accounts the
// selected row's host holds, current one marked, Enter applies immediately.
//
// Style follows the existing small overlays (confirm_overlay.go) rather than the
// full searchable resume picker: a host holds two or three accounts, so there is
// nothing to search. Everything it shows comes from data the pollers already
// hold — opening the picker never triggers a fetch.

// accountPickerHint is the fixed hint row drawn below the account rows.
const accountPickerHint = "↑/↓ select    ⏎ switch    esc cancel"

// accountPickerEmpty is shown for a host with no snapshots at all. Only Esc is
// live in that state — there is nothing to select.
const accountPickerEmpty = "no known account snapshots"

// accountPickerEmptyHint tells the user how to create one, naming the command
// that does it.
const accountPickerEmptyHint = "claude-sessions account claude save <name>  (or grok)"

// accountActiveGlyph marks the account the host is currently logged into.
const accountActiveGlyph = "●"

// accountPickerState is the picker's pure key-handling core: which row is
// selected, and what a keystroke does to it.
type accountPickerState struct {
	sel  int
	rows int
}

// handle applies one key event. confirm means "apply the selected row"; cancel
// means "close, change nothing". Any other key (including nav on an empty list)
// just redraws, so an unmapped keystroke never dismisses the overlay.
func (s *accountPickerState) handle(key string) (confirm, cancel bool) {
	switch key {
	case KeyEsc, "q", "Q", "\x03", "\x04":
		return false, true
	case KeyUp:
		if s.rows > 0 {
			s.sel = (s.sel - 1 + s.rows) % s.rows
		}
		return false, false
	case KeyDown:
		if s.rows > 0 {
			s.sel = (s.sel + 1) % s.rows
		}
		return false, false
	case KeyEnter, "\r", "\n":
		if s.rows == 0 {
			return false, false
		}
		return true, false
	default:
		return false, false
	}
}

// accountPickerTitle names the host whose accounts are listed. An empty host is
// this machine.
func accountPickerTitle(host string) string {
	if host == "" {
		return "switch account · this host"
	}
	return "switch account · " + host
}

// accountPickerRow is one rendered line: the active marker, the snapshot name,
// then its email. The marker column is always present so names line up whether
// or not the active account is known.
func accountPickerRow(row accountRow, nameW int) string {
	marker := " "
	if row.Active {
		marker = accountActiveGlyph
	}
	tool := row.Tool
	if tool == "" {
		tool = "-"
	}
	return fmt.Sprintf("%-6s %s %-*s  %s", tool, marker, nameW, row.Name, dim(displayEmail(row.Email)))
}

// renderAccountPicker draws the bordered box centered in a cols x rows terminal:
// the title, one line per account, a blank separator, then the dimmed hint.
// Positioning, clipping and the unknown-size fallback all match
// renderConfirmOverlay, so the two overlays look like the same dialog.
func renderAccountPicker(host string, accounts []accountRow, sel, cols, rows int) string {
	title := accountPickerTitle(host)
	nameW := 0
	for _, a := range accounts {
		if n := visualLen(a.Name); n > nameW {
			nameW = n
		}
	}

	body := make([]string, 0, len(accounts)+1)
	hint := accountPickerHint
	if len(accounts) == 0 {
		body = append(body, dim(accountPickerEmpty), dim(accountPickerEmptyHint))
		hint = "esc close"
	}
	for _, a := range accounts {
		body = append(body, accountPickerRow(a, nameW))
	}

	innerWidth := visualLen(title)
	for _, l := range append(append([]string{}, body...), hint) {
		if w := visualLen(l); w > innerWidth {
			innerWidth = w
		}
	}
	if cols > 0 {
		max := cols - 4 // border + one space of padding each side
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	pad := func(s string, selected bool) string {
		s = clipLine(s, innerWidth)
		line := s + strings.Repeat(" ", innerWidth-visualLen(s))
		if selected {
			line = highlightSelectedRow(line, true)
		}
		return confirmBoxV + " " + line + " " + confirmBoxV
	}

	box := make([]string, 0, len(body)+5)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	box = append(box, pad(bold(title), false))
	box = append(box, pad("", false))
	for i, l := range body {
		box = append(box, pad(l, len(accounts) > 0 && i == sel))
	}
	box = append(box, pad("", false))
	box = append(box, pad(dim(hint), false))
	box = append(box, confirmBoxBL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxBR)

	if cols <= 0 || rows <= 0 {
		return strings.Join(box, "\n")
	}

	boxWidth := innerWidth + 4
	left := (cols - boxWidth) / 2
	if left < 0 {
		left = 0
	}
	leftPad := strings.Repeat(" ", left)
	for i, l := range box {
		box[i] = leftPad + l
	}
	top := (rows - len(box)) / 2
	if top < 0 {
		top = 0
	}
	lines := make([]string, 0, top+len(box))
	for i := 0; i < top; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, box...)
	return strings.Join(lines, "\n")
}

// pickAccount drives the blocking picker, mirroring confirmOverlayPreview's
// read/handle loop. Must be called in raw mode; it never leaves raw or the
// alt-screen, so the caller's next render() paints over it. wakes carries the
// modal wake sources (resize) so the box stays centered across a live resize.
//
// The selection starts on the active account, so Enter without moving is the
// no-op the caller turns into "nothing to do".
func pickAccount(host string, accounts []accountRow, wakes []wakeFD) (accountRow, bool) {
	state := accountPickerState{rows: len(accounts)}
	for i, a := range accounts {
		if a.Active {
			state.sel = i
			break
		}
	}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderAccountPicker(host, accounts, state.sel, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			confirm, cancel := state.handle(key)
			if cancel {
				return accountRow{}, false
			}
			if confirm {
				return accounts[state.sel], true
			}
		}
	}
}

// accountSwitchOutcome is what a Ctrl+W did, for the main loop: the toast
// (empty for "nothing happened" or a failure the error panel already
// showed), whether it warrants the amber 8 s style, and whether a switch
// actually landed.
type accountSwitchOutcome struct {
	Toast    string
	Warn     bool
	Switched bool
}

// actSwitchAccount handles Ctrl+W on the selected row: pick one of that row's
// host's accounts and apply it. Local rows switch in-process; remote rows post to
// that host's own server, the same split actKillRemote/actAttachRemote follow.
//
// Switched tells the caller to refresh and kick the account pollers — their
// snapshots still describe the previous account until they refetch. After a
// switch FLEET is told (c.notifyFleet), and the toast says whether it was. A
// failed switch opens the error panel, which stays until dismissed.
//
// Enter on the already-active account is a true no-op: no request is sent at all,
// and the overlay closes cleanly. There is no confirmation step by design —
// picker plus Enter is the whole interaction.
func actSwitchAccount(c *actCtx) accountSwitchOutcome {
	target := c.selectedTarget()
	if target == nil || c.accounts == nil {
		return accountSwitchOutcome{}
	}
	host := target.host
	label := localAccountHost
	if host != "" {
		label = host
	}
	rows := hostAccountRows(label, host, c.accounts)
	choice, ok := pickAccount(host, rows, c.modalWakes)
	if !ok || choice.Active {
		return accountSwitchOutcome{}
	}

	// The switch itself blocks, and can block for a while: on macOS it shells
	// out to `security`, which may sit on a Keychain dialog, and a remote switch
	// is an HTTP round trip. Drop to cooked mode behind a status line first — the
	// same thing actKillRemote does before its request — rather than leaving the
	// UI frozen and silent with no explanation. Telling FLEET runs in the same
	// window: discovery plus one short POST.
	var (
		email    string
		warnings []string
		err      error
		notice   *fleetSwitchNotice
	)
	func() {
		c.prepareLineOutput()
		defer c.enterRaw()
		fmt.Printf("\nswitching %s to %s... ", label, choice.Name)
		email, warnings, err = applyAccountSwitch(host, choice.Name, choice.Tool)
		if err != nil {
			fmt.Println("failed")
			return
		}
		fmt.Println("ok")
		if c.notifyFleet != nil {
			fmt.Print("telling FLEET... ")
			n := c.notifyFleet(accountSwitchEventRequest(host, choice, rows))
			notice = &n
			if n.Told {
				fmt.Println("told")
			} else {
				fmt.Println("not told")
			}
		}
	}()
	if err != nil {
		showAccountSwitchError(label, choice.Tool, choice.Name, err.Error(), c.modalWakes)
		return accountSwitchOutcome{}
	}
	toast, warn := accountSwitchResultToast(accountSwitchToast(label, choice.Name, email, warnings), len(warnings) > 0, notice)
	return accountSwitchOutcome{Toast: toast, Warn: warn, Switched: true}
}

// accountSwitchEventRequest is the event the TUI sends FLEET after switching
// choice's tool on host ("" is this machine, named by its short hostname).
// From is the account the picker marked active for that tool, "" when none.
func accountSwitchEventRequest(host string, choice accountRow, rows []accountRow) fleetAccountEventRequest {
	if host == "" {
		host = shortHostname()
	}
	req := fleetAccountEventRequest{Host: host, Tool: choice.Tool, To: choice.Name}
	for _, r := range rows {
		if r.Active && r.Tool == choice.Tool {
			req.From = r.Name
			break
		}
	}
	return req
}

// fleetSwitchNotice is how telling FLEET about a switch went: Told, or the
// short Reason it was not.
type fleetSwitchNotice struct {
	Told   bool
	Reason string
}

// fleetAccountEventVia is the TUI's notifyFleet: a fresh discovery (this
// host's service first, then the servers.yaml hosts), then one POST to the
// host that runs FLEET. It runs inside the Ctrl+W action, never on the render
// path.
func fleetAccountEventVia(src *fleetLocal) func(fleetAccountEventRequest) fleetSwitchNotice {
	return func(req fleetAccountEventRequest) fleetSwitchNotice {
		v, err := src.State()
		var down *fleetHostDownError
		switch {
		case errors.As(err, &down):
			return fleetSwitchNotice{Reason: "FLEET host down"}
		case err != nil:
			return fleetSwitchNotice{Reason: "no FLEET found"}
		case len(v.Ambiguous) > 0:
			return fleetSwitchNotice{Reason: "two FLEETs live"}
		case !v.Fleet.Alive:
			return fleetSwitchNotice{Reason: "no FLEET found"}
		}
		c, err := src.Client()
		if err != nil || c.base != v.Source {
			return fleetSwitchNotice{Reason: "FLEET moved"}
		}
		return fleetSwitchNoticeFor(c.AccountEvent(req))
	}
}

// fleetSwitchNoticeFor reads AccountEvent's answer: "sent" is told; a
// delivery refusal or a failed request is a short reason.
func fleetSwitchNoticeFor(code string, err error) fleetSwitchNotice {
	if err != nil {
		switch fleetErrorStatus(err) {
		case 0:
			return fleetSwitchNotice{Reason: "request failed"}
		case http.StatusNotFound:
			return fleetSwitchNotice{Reason: "FLEET server too old"}
		case http.StatusUnauthorized:
			return fleetSwitchNotice{Reason: "token rejected"}
		default:
			return fleetSwitchNotice{Reason: "request failed"}
		}
	}
	if code == "sent" {
		return fleetSwitchNotice{Told: true}
	}
	return fleetSwitchNotice{Reason: fleetDeliveryReason(code)}
}

// fleetDeliveryReason maps idleDeliver's refusal codes to the toast's short
// phrase. An unknown code shows as itself, cut short.
func fleetDeliveryReason(code string) string {
	switch code {
	case "target_missing":
		return "no FLEET found"
	case "target_ambiguous":
		return "two FLEETs live"
	case "list_failed":
		return "session list failed"
	case "busy":
		return "FLEET busy"
	case "dialog":
		return "FLEET shows a dialog"
	case "input_busy":
		return "FLEET input not empty"
	case "pane_mode":
		return "FLEET pane in scroll mode"
	case "no_pane":
		return "no FLEET pane"
	case "not_submitted":
		return "typed, Enter not confirmed"
	case "double_tap", "rate_limited":
		return "rate limited"
	case "send_failed":
		return "send failed"
	case "":
		return "no answer"
	}
	return "refused: " + truncateRunes(code, 24)
}

// accountSwitchResultToast folds the FLEET notice into a success toast. warn
// (amber, 8 s) is a switch warning or FLEET not told; a nil notice (no
// notifier) adds nothing.
func accountSwitchResultToast(base string, warnings bool, n *fleetSwitchNotice) (string, bool) {
	warn := warnings
	switch {
	case n == nil:
	case n.Told:
		base += " · FLEET told"
	default:
		base += " · FLEET not told (" + n.Reason + ")"
		warn = true
	}
	return base, warn
}

// applyAccountSwitch performs the switch on the row's own host: in-process for a
// local row, over that host's HTTP API for a remote one — the same local/remote
// split every other action takes. Returns the email now live there, plus any
// advisory warnings that host raised about the switch.
func hostAccountRows(label, host string, accounts func(string) accountSnapshot) []accountRow {
	var snap accountSnapshot
	if accounts != nil {
		snap = accounts(host)
	}
	claude := accountRowsFrom(label, snap)
	var grok []accountRow
	if host == "" {
		grok = localGrokAccountListing().Rows
	} else {
		grok = grokAccountRowsFrom(label, snap)
	}
	return append(claude, grok...)
}

func applyAccountSwitch(host, name, tool string) (string, []string, error) {
	if tool != accountToolClaude && tool != accountToolGrok {
		return "", nil, fmt.Errorf("tool is required (claude or grok)")
	}
	if host == "" {
		if tool == accountToolGrok {
			return switchGrokAccount(name)
		}
		return switchAccount(name)
	}
	result, err := switchAccountRemote(host, name, tool)
	if err != nil {
		return "", nil, err
	}
	if !result.OK {
		return "", nil, fmt.Errorf("%s", result.Message)
	}
	return result.Account, result.Warnings, nil
}

// accountSwitchToast is the one-liner shown after a successful switch. A switch
// only ever succeeds with a real, non-empty email (see accountSwitchedLine in
// commands.go for why), so there is nothing to omit here.
//
// A warning is folded in as a short clause rather than printed separately. The
// cooked window this action opens is repainted the instant the TUI loop resumes,
// so anything written there is gone before it can be read — the toast is the
// only surface that survives. The full text stays on the CLI path
// (printAccountWarnings), which has a scrollback to land in.
func accountSwitchToast(host, name, email string, warnings []string) string {
	line := fmt.Sprintf("%s: switched to %s (%s)", host, name, email)
	if len(warnings) == 0 {
		return line
	}
	return line + " — warning: running Claude sessions may overwrite this credential"
}
