package main

// The 's'/'S' sort-mode dialog: a small bordered overlay listing every entry
// in sortModeOrder, current mode marked, Enter applies immediately.
//
// Style follows the existing small overlays (account_picker.go /
// confirm_overlay.go) rather than the full searchable resume picker: there
// are exactly 6 fixed modes, so there is nothing to search.

// sortPickerState is the picker's pure key-handling core: which row is
// selected, and what a keystroke does to it. Structurally identical to
// accountPickerState (account_picker.go) — same four-key contract.
type sortPickerState struct {
	sel  int
	rows int
}

// handle applies one key event. confirm means "apply the selected row";
// cancel means "close, change nothing". Any other key (including nav on an
// empty list) just redraws, so an unmapped keystroke never dismisses the
// overlay. rows is always len(sortModeOrder) (6) in production, never 0 —
// the rows==0 guard is kept anyway since sortModeOrder is a mutable package
// var, not a const, so a future zero-value construction should redraw
// instead of panicking on an out-of-range index.
func (s *sortPickerState) handle(key string) (confirm, cancel bool) {
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
