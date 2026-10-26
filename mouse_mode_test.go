package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestMouseSelectKey(t *testing.T) {
	var m mouseModeState
	var b strings.Builder
	if !m.wanted() {
		t.Fatal("mouse is off by default")
	}
	if m.selectKey(&b, KeyEsc) || b.Len() != 0 {
		t.Fatalf("Esc outside select mode was used (wrote %q)", b.String())
	}
	if m.selectKey(&b, "x") {
		t.Fatal("x was used")
	}
	if !m.selectKey(&b, "v") || !m.selectOn || m.wanted() || b.String() != mouseDisableSequence {
		t.Fatalf("v: on=%v wanted=%v wrote %q", m.selectOn, m.wanted(), b.String())
	}
	if m.selectHint() == "" {
		t.Fatal("no hint in select mode")
	}
	b.Reset()
	if !m.selectKey(&b, KeyEsc) || m.selectOn || !m.wanted() || b.String() != mouseEnableSequence {
		t.Fatalf("Esc: on=%v wrote %q", m.selectOn, b.String())
	}
	if m.selectHint() != "" {
		t.Fatal("hint outside select mode")
	}
	b.Reset()
	m.selectKey(&b, "v")
	m.selectKey(&b, "v")
	if m.selectOn || b.String() != mouseDisableSequence+mouseEnableSequence {
		t.Fatalf("v v: on=%v wrote %q", m.selectOn, b.String())
	}
}

func TestMouseSuspend(t *testing.T) {
	var m mouseModeState
	var b strings.Builder
	resume := m.suspend(&b)
	if m.wanted() || b.String() != mouseDisableSequence {
		t.Fatalf("suspend: wanted=%v wrote %q", m.wanted(), b.String())
	}
	if m.selectHint() != "" {
		t.Fatal("a suspension is not select mode")
	}
	b.Reset()
	resume()
	if !m.wanted() || b.String() != mouseEnableSequence {
		t.Fatalf("resume: wanted=%v wrote %q", m.wanted(), b.String())
	}
	// Select mode outlives a suspension: resuming leaves the mouse off.
	m.selectKey(&b, "v")
	resume = m.suspend(&b)
	b.Reset()
	resume()
	if m.wanted() || b.String() != mouseDisableSequence {
		t.Fatalf("resume in select mode: wanted=%v wrote %q", m.wanted(), b.String())
	}
}

// withMouseMode sets the global mouse state for one test.
func withMouseMode(t *testing.T, m mouseModeState) {
	t.Helper()
	prev := mouseMode
	mouseMode = m
	t.Cleanup(func() { mouseMode = prev })
}

func TestActCtxEnterRawKeepsSelectMode(t *testing.T) {
	var buf bytes.Buffer
	prev := terminalOutput
	terminalOutput = &buf
	t.Cleanup(func() { terminalOutput = prev })
	withMouseMode(t, mouseModeState{selectOn: true})

	(&actCtx{fd: -1}).enterRaw()
	if strings.Contains(buf.String(), mouseEnableSequence) || !strings.Contains(buf.String(), mouseDisableSequence) {
		t.Fatalf("enterRaw in select mode wrote %q", buf.String())
	}

	buf.Reset()
	mouseMode = mouseModeState{suspended: 1}
	(&actCtx{fd: -1}).enterRaw()
	if strings.Contains(buf.String(), mouseEnableSequence) {
		t.Fatalf("enterRaw under a suspension wrote %q", buf.String())
	}

	// An attach from the Fleet view comes back with capture on, even when
	// the list it was opened from is in select mode.
	buf.Reset()
	mouseMode = mouseModeState{selectOn: true, captured: 1}
	(&actCtx{fd: -1}).enterRaw()
	if !strings.Contains(buf.String(), mouseEnableSequence) || strings.Contains(buf.String(), mouseDisableSequence) {
		t.Fatalf("enterRaw in the Fleet view wrote %q", buf.String())
	}
}

// The Fleet view captures the mouse for its whole life, whatever the list's
// select mode; its raw text screen suspends it inside that; closing the view
// gives the list back its own mode.
func TestMouseCapture(t *testing.T) {
	for _, selectOn := range []bool{false, true} {
		m := mouseModeState{selectOn: selectOn}
		var b strings.Builder
		release := m.capture(&b)
		if !m.wanted() || b.String() != mouseEnableSequence {
			t.Fatalf("select=%v capture: wanted=%v wrote %q", selectOn, m.wanted(), b.String())
		}
		b.Reset()
		resume := m.suspend(&b)
		if m.wanted() || b.String() != mouseDisableSequence {
			t.Fatalf("select=%v raw view: wanted=%v wrote %q", selectOn, m.wanted(), b.String())
		}
		b.Reset()
		resume()
		if !m.wanted() || b.String() != mouseEnableSequence {
			t.Fatalf("select=%v raw view back: wanted=%v wrote %q", selectOn, m.wanted(), b.String())
		}
		b.Reset()
		release()
		want := mouseEnableSequence
		if selectOn {
			want = mouseDisableSequence
		}
		if m.wanted() == selectOn || b.String() != want || m.selectOn != selectOn {
			t.Fatalf("select=%v close: wanted=%v wrote %q", selectOn, m.wanted(), b.String())
		}
	}
}

func TestSelectModeHints(t *testing.T) {
	var m mouseModeState
	m.selectKey(&strings.Builder{}, "v")
	hint := m.selectHint()
	got := sessionBottomRow("", false, hint)
	if !strings.Contains(got, "SELECT MODE") || strings.Contains(got, "disable/enable") {
		t.Fatalf("bottom row = %q", got)
	}
	if got := sessionBottomRow("sorted", true, hint); !strings.Contains(got, "sorted") {
		t.Fatalf("a toast still wins: %q", got)
	}

	withMouseMode(t, m)
	if got := inspectorFooterRight(inspectorViewState{}); !strings.Contains(got, "SELECT") {
		t.Fatalf("inspector footer = %q", got)
	}
	if help := renderHelp("dir", false); !strings.Contains(help, "select mode") || !strings.Contains(help, "Option+drag") {
		t.Fatalf("help lacks select mode: %q", help)
	}
}

func TestFleetRawTextKey(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now)
	if cmd := st.handleKey("t", false, now); cmd != fleetCmdRawText {
		t.Fatalf("t = %v, want raw text", cmd)
	}
	if cmd := st.handleKey("t", true, now); cmd != fleetCmdNone {
		t.Fatalf("t in a paste = %v, want none", cmd)
	}
	empty := fleetTestState(fleetTestView(), now)
	if cmd := empty.handleKey("t", false, now); cmd != fleetCmdNone {
		t.Fatalf("t with no card = %v", cmd)
	}
	if !strings.Contains(fleetHelpRow, "t raw text") || !strings.Contains(fleetHelpRowAttach, "t raw text") {
		t.Fatal("help rows lack t")
	}
}
