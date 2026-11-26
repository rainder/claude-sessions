package main

import (
	"strings"
	"testing"
)

// composeState is an inspector on session pid (tmux pane present), not
// composing yet.
func composeState(pid int) *tuiState {
	return &tuiState{mode: screenInspector, inspector: inspectorViewState{
		snapshot: InspectorSnapshot{Session: Session{PID: pid, Tmux: "work:0.0"}},
	}}
}

func composeKey(t *testing.T, state *tuiState, key string, send func(Session, string) (bool, string)) (quit, absorb bool) {
	t.Helper()
	var hub *InspectorHub
	noop := func() {}
	return handleInspectorEvent(inputEvent{kind: eventKey, key: key}, false, state, &hub, noop, noop, noop, noop, send)
}

// Esc closes the compose box and keeps the draft for that session; i on the
// same session brings it back, another session starts empty; Ctrl-C closes
// and clears; a sent message clears.
func TestInspectorComposeDraftKeptPerSession(t *testing.T) {
	state := composeState(42)
	composeKey(t, state, "i", nil)
	for _, k := range []string{"h", "i"} {
		composeKey(t, state, k, nil)
	}
	if quit, absorb := composeKey(t, state, KeyEsc, nil); quit || !absorb || state.inspector.composing {
		t.Fatalf("Esc: quit=%v absorb=%v composing=%v", quit, absorb, state.inspector.composing)
	}
	if state.composeDrafts["42"] != "hi" {
		t.Fatalf("drafts %v", state.composeDrafts)
	}
	composeKey(t, state, "i", nil)
	if !state.inspector.composing || state.inspector.compose.Text() != "hi" {
		t.Fatalf("reopen: composing=%v text %q", state.inspector.composing, state.inspector.compose.Text())
	}

	// Another session (the inspector struct is replaced on open) starts empty.
	other := composeState(7)
	other.composeDrafts = state.composeDrafts
	composeKey(t, other, "i", nil)
	if other.inspector.compose.Text() != "" {
		t.Fatalf("other session got %q", other.inspector.compose.Text())
	}

	// Ctrl-C closes and clears; the next open is empty.
	if quit, absorb := composeKey(t, state, "\x03", nil); quit || !absorb || state.inspector.composing {
		t.Fatalf("Ctrl-C: quit=%v absorb=%v", quit, absorb)
	}
	if _, ok := state.composeDrafts["42"]; ok {
		t.Fatal("Ctrl-C kept the draft")
	}
	composeKey(t, state, "i", nil)
	if state.inspector.compose.Text() != "" {
		t.Fatalf("after Ctrl-C: %q", state.inspector.compose.Text())
	}

	// A sent message clears the draft; a failed one keeps the box.
	composeKey(t, state, "x", nil)
	composeKey(t, state, KeyEsc, nil)
	composeKey(t, state, "i", nil)
	fail := func(Session, string) (bool, string) { return false, "nope" }
	composeKey(t, state, KeyEnter, fail)
	if !state.inspector.composing || state.inspector.compose.Text() != "x" {
		t.Fatal("a failed send lost the box")
	}
	var sent string
	ok := func(_ Session, text string) (bool, string) { sent = text; return true, "" }
	composeKey(t, state, KeyEnter, ok)
	if sent != "x" || state.inspector.composing {
		t.Fatalf("send: %q composing=%v", sent, state.inspector.composing)
	}
	if _, kept := state.composeDrafts["42"]; kept {
		t.Fatal("a sent message stayed a draft")
	}
	composeKey(t, state, "i", nil)
	if state.inspector.compose.Text() != "" {
		t.Fatalf("after send: %q", state.inspector.compose.Text())
	}
	// A whitespace-only box is no draft.
	composeKey(t, state, " ", nil)
	composeKey(t, state, KeyEsc, nil)
	if _, kept := state.composeDrafts["42"]; kept {
		t.Fatal("whitespace kept as a draft")
	}
}

// The compose box draws its key hint above the box, and the frame keeps
// exactly rows lines.
func TestRenderInspectorComposeHint(t *testing.T) {
	v := populatedInspectorView()
	v.composing = true
	v.compose = editorWith("one\ntwo")
	var b strings.Builder
	RenderInspector(&b, v, nil, false, 100, 20)
	lines := inspectorFrameLines(b.String())
	if len(lines) != 20 {
		t.Fatalf("frame has %d lines", len(lines))
	}
	if got := stripANSI(lines[17]); got != composeHint {
		t.Fatalf("hint row = %q", got)
	}
	if !strings.Contains(lines[18], "> one") || !strings.Contains(lines[19], "two") {
		t.Fatalf("box rows = %q / %q", lines[18], lines[19])
	}
	v.compose = editorWith("x")
	for _, rows := range []int{6, 8} {
		var b strings.Builder
		RenderInspector(&b, v, nil, false, 60, rows)
		lines := inspectorFrameLines(b.String())
		if len(lines) != rows {
			t.Fatalf("rows=%d: frame has %d lines", rows, len(lines))
		}
		// The hint never takes the only body row: row 3 is body.
		if stripANSI(lines[rows-2]) != clipLine(composeHint, 60) || stripANSI(lines[3]) == clipLine(composeHint, 60) {
			t.Fatalf("rows=%d: hint %q body %q", rows, lines[rows-2], lines[3])
		}
	}
	// One row fewer is the too-small notice, not a frame with no body.
	var b5 strings.Builder
	RenderInspector(&b5, v, nil, false, 60, minInspectorRows-1)
	if !strings.Contains(b5.String(), "terminal too small") {
		t.Fatalf("rows=%d: %q", minInspectorRows-1, b5.String())
	}
}

func TestTextFilterClear(t *testing.T) {
	if next, changed := textFilterClear(textFilterState{committed: "api"}); !changed || next != (textFilterState{}) {
		t.Fatalf("committed: %+v %v", next, changed)
	}
	if _, changed := textFilterClear(textFilterState{}); changed {
		t.Fatal("no filter: changed")
	}
	editing := textFilterState{editing: true, buffer: "a", committed: "b"}
	if next, changed := textFilterClear(editing); changed || next != editing {
		t.Fatal("editing: changed")
	}
}

func TestNavTargetsClamped(t *testing.T) {
	targets := []selectionTarget{{id: "a"}, {id: "b"}, {id: "c"}}
	cases := []struct {
		sel   string
		delta int
		want  string
	}{
		{"a", -1, "a"}, {"a", 1, "b"}, {"a", 10, "c"}, {"c", 1, "c"}, {"c", -10, "a"},
		{"", 1, "a"}, {"", -1, "c"}, {"gone", 5, "a"},
	}
	for _, c := range cases {
		if got := navTargetsClamped(targets, c.sel, c.delta); got != c.want {
			t.Errorf("navTargetsClamped(%q, %d) = %q, want %q", c.sel, c.delta, got, c.want)
		}
	}
	if got := navTargetsClamped(nil, "a", 1); got != "" {
		t.Errorf("empty = %q", got)
	}
}

// A page moves by what the last frame showed on one screen, never wraps,
// and always moves at least one row.
func TestPageTarget(t *testing.T) {
	var targets []selectionTarget
	var rows []tableRow
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		targets = append(targets, selectionTarget{id: id})
		// A heading line every third row, as host and group headings do.
		rows = append(rows, tableRow{line: 2 + i + i/3, targetID: id})
	}
	cases := []struct {
		sel      string
		viewRows int
		dir      int
		want     string
	}{
		{"a", 4, 1, "c"},  // lines 2→5: c (4) fits, d (6) does not
		{"c", 4, 1, "e"},  // 4→7: d (6), e (7)
		{"e", 4, 1, "f"},  // clamped at the last row
		{"f", 4, 1, "f"},  // no wrap
		{"f", 4, -1, "d"}, // 8→5: e (7), d (6)
		{"a", 4, -1, "a"}, // no wrap
		{"a", 1, 1, "b"},  // a one-line screen still moves
		{"a", 100, 1, "f"},
	}
	for _, c := range cases {
		if got := pageTarget(targets, rows, c.viewRows, c.sel, c.dir); got != c.want {
			t.Errorf("pageTarget(%q, view %d, dir %d) = %q, want %q", c.sel, c.viewRows, c.dir, got, c.want)
		}
	}
	// Before any frame: pages by viewRows-1 targets, clamped.
	if got := pageTarget(targets, nil, 3, "a", 1); got != "c" {
		t.Errorf("no frame = %q", got)
	}
	if got := pageTarget(nil, rows, 4, "a", 1); got != "" {
		t.Errorf("no targets = %q", got)
	}

	// page records explicit navigation like navigate does.
	st := &tuiState{sel: "a", pending: &pendingSpawn{tmux: "x"}, listRows: rows, listViewRows: 4}
	st.page(targets, 1)
	if st.sel != "c" || st.pending != nil || !st.anchorSelection {
		t.Fatalf("page: sel %q pending %v anchor %v", st.sel, st.pending, st.anchorSelection)
	}
}

// PgUp/PgDn move a page and Home/End to the ends in the resume picker;
// letters (g, G) stay filter text.
func TestResumePickerStatePageKeys(t *testing.T) {
	state := resumePickerState{RowCount: 30, Page: 8}
	state.handle(KeyPageDown)
	if state.Row != 8 {
		t.Fatalf("PgDn = %d", state.Row)
	}
	state.handle(KeyEnd)
	if state.Row != 29 {
		t.Fatalf("End = %d", state.Row)
	}
	state.handle(KeyPageDown)
	if state.Row != 29 {
		t.Fatalf("PgDn at the end = %d (no wrap)", state.Row)
	}
	state.handle(KeyPageUp)
	if state.Row != 21 {
		t.Fatalf("PgUp = %d", state.Row)
	}
	state.handle(KeyHome)
	state.handle(KeyPageUp)
	if state.Row != 0 {
		t.Fatalf("PgUp at the top = %d", state.Row)
	}
	unsized := resumePickerState{RowCount: 30}
	unsized.handle(KeyPageDown)
	if unsized.Row != 10 {
		t.Fatalf("PgDn before a draw = %d", unsized.Row)
	}
	empty := resumePickerState{}
	for _, k := range []string{KeyPageDown, KeyEnd, KeyPageUp, KeyHome} {
		empty.handle(k)
		if empty.Row != 0 {
			t.Fatalf("%q on no rows = %d", k, empty.Row)
		}
	}
	state.handle("g")
	state.handle("G")
	if state.Filter != "gG" {
		t.Fatalf("filter = %q", state.Filter)
	}
}
