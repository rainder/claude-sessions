package main

import (
	"fmt"
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
const accountPickerEmptyHint = "claude-sessions account save <name>"

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
	return fmt.Sprintf("%s %-*s  %s", marker, nameW, row.Name, dim(displayEmail(row.Email)))
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

// actSwitchAccount handles Ctrl+W on the selected row: pick one of that row's
// host's accounts and apply it. Local rows switch in-process; remote rows post to
// that host's own server, the same split actKillRemote/actAttachRemote follow.
//
// Returns the toast to show (empty for "nothing happened") and whether a switch
// actually landed, so the caller can refresh and kick the account pollers — their
// snapshots still describe the previous account until they refetch.
//
// Enter on the already-active account is a true no-op: no request is sent at all,
// and the overlay closes cleanly. There is no confirmation step by design —
// picker plus Enter is the whole interaction.
func actSwitchAccount(c *actCtx) (toast string, switched bool) {
	target := c.selectedTarget()
	if target == nil || c.accounts == nil {
		return "", false
	}
	host := target.host
	label := localAccountHost
	if host != "" {
		label = host
	}
	choice, ok := pickAccount(host, accountRowsFrom(label, c.accounts(host)), c.modalWakes)
	if !ok || choice.Active {
		return "", false
	}

	// The switch itself blocks, and can block for a while: on macOS it shells
	// out to `security`, which may sit on a Keychain dialog, and a remote switch
	// is an HTTP round trip. Drop to cooked mode behind a status line first — the
	// same thing actKillRemote does before its request — rather than leaving the
	// UI frozen and silent with no explanation.
	c.prepareLineOutput()
	defer c.enterRaw()
	fmt.Printf("\nswitching %s to %s... ", label, choice.Name)

	email, err := applyAccountSwitch(host, choice.Name)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		return "account switch failed: " + err.Error(), false
	}
	fmt.Println("ok")
	return accountSwitchToast(label, choice.Name, email), true
}

// applyAccountSwitch performs the switch on the row's own host: in-process for a
// local row, over that host's HTTP API for a remote one — the same local/remote
// split every other action takes. Returns the email now live there.
func applyAccountSwitch(host, name string) (string, error) {
	if host == "" {
		return switchAccount(name)
	}
	result, err := switchAccountRemote(host, name)
	if err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("%s", result.Message)
	}
	return result.Account, nil
}

// accountSwitchToast is the one-liner shown after a successful switch. A switch
// only ever succeeds with a real, non-empty email (see accountSwitchedLine in
// commands.go for why), so there is nothing to omit here.
func accountSwitchToast(host, name, email string) string {
	return fmt.Sprintf("%s: switched to %s (%s)", host, name, email)
}
