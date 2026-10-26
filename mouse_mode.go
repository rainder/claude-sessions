package main

import "io"

// mouseModeState is whether the TUI wants terminal mouse reporting. While it is
// on, the terminal sends clicks and drags to the TUI, so a drag cannot select
// text. Three things decide it, strongest first:
//
//   - suspended: a screen that reads no mouse events (the Fleet view's raw
//     text screen) turns it off. It is not a user choice, so it never shows
//     the select-mode hint.
//   - captured: a screen that reads the mouse and has no select mode (the
//     Fleet view) turns it on, even over the list's select mode.
//   - selectOn: select mode, the v key on the session list and the inspector,
//     turns it off. The user turns it off with v again or with Esc.
//
// Every place that re-enters the TUI after a prompt or a subprocess writes
// writeMouseMode(w, mouseMode.wanted()), never a bare enable or disable, so an
// attach comes back in the mode of the screen it left.
type mouseModeState struct {
	selectOn  bool
	suspended int
	captured  int
}

// mouseMode is the TUI's one mouse state. RunTUI owns the terminal, so there
// is exactly one.
var mouseMode mouseModeState

// wanted reports whether mouse reporting should be on now.
func (m *mouseModeState) wanted() bool {
	return m.suspended == 0 && (m.captured > 0 || !m.selectOn)
}

// selectKey applies a key to select mode and reports whether it used the key:
// v toggles select mode, and Esc ends it (only while it is on, so Esc keeps its
// other meanings otherwise). It writes the new mouse mode to w.
func (m *mouseModeState) selectKey(w io.Writer, k string) bool {
	switch {
	case k == "v" || k == "V":
		m.selectOn = !m.selectOn
	case k == KeyEsc && m.selectOn:
		m.selectOn = false
	default:
		return false
	}
	writeMouseMode(w, m.wanted())
	return true
}

// suspend turns mouse reporting off for a screen that reads no mouse events,
// and returns the call that ends the suspension and writes the mode back.
func (m *mouseModeState) suspend(w io.Writer) (resume func()) {
	m.suspended++
	writeMouseMode(w, false)
	return func() {
		m.suspended--
		writeMouseMode(w, m.wanted())
	}
}

// capture turns mouse reporting on for a screen that reads the mouse and has
// no select mode of its own, and returns the call that ends the capture and
// writes the mode back (the list's select mode, if it had one).
func (m *mouseModeState) capture(w io.Writer) (release func()) {
	m.captured++
	writeMouseMode(w, m.wanted())
	return func() {
		m.captured--
		writeMouseMode(w, m.wanted())
	}
}

// selectHint is the select-mode status line text, "" when select mode is off.
func (m *mouseModeState) selectHint() string {
	if !m.selectOn {
		return ""
	}
	return "SELECT MODE · mouse off: drag selects text, Cmd+C copies · v or Esc: mouse on"
}
