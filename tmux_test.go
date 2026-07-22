package main

import "testing"

func TestParseTmuxPaneOutput(t *testing.T) {
	got := parseTmuxPaneOutput(
		"101\talpha beta:0.1\t0\n" +
			"102\tsolo:1.2\t1\n" +
			"103\tpairing:2.3\t4\n" +
			"104\tcrowd:0.0\t12\n" +
			"105\tunknown:0.0\tbogus\n" +
			"106\tnegative:0.0\t-1\n" +
			"bad\tignored:0.0\t1\n" +
			"0\tignored-zero:0.0\t1\n" +
			"-2\tignored-negative:0.0\t1\n" +
			"107\t\t1\n" +
			"missing-fields\n",
	)

	if len(got) != 6 {
		t.Fatalf("parseTmuxPaneOutput returned %d panes, want 6: %#v", len(got), got)
	}

	cases := []struct {
		pid          int
		wantLocation string
		wantAttached *int
	}{
		{101, "alpha beta:0.1", intPtrForTmuxTest(0)},
		{102, "solo:1.2", intPtrForTmuxTest(1)},
		{103, "pairing:2.3", intPtrForTmuxTest(4)},
		{104, "crowd:0.0", intPtrForTmuxTest(12)},
		{105, "unknown:0.0", nil},
		{106, "negative:0.0", nil},
	}
	for _, tc := range cases {
		info, ok := got[tc.pid]
		if !ok {
			t.Fatalf("pane %d missing from %#v", tc.pid, got)
		}
		if info.Location != tc.wantLocation {
			t.Errorf("pane %d location = %q, want %q", tc.pid, info.Location, tc.wantLocation)
		}
		switch {
		case tc.wantAttached == nil && info.Attached != nil:
			t.Errorf("pane %d attached = %d, want nil", tc.pid, *info.Attached)
		case tc.wantAttached != nil && info.Attached == nil:
			t.Errorf("pane %d attached = nil, want %d", tc.pid, *tc.wantAttached)
		case tc.wantAttached != nil && *info.Attached != *tc.wantAttached:
			t.Errorf("pane %d attached = %d, want %d", tc.pid, *info.Attached, *tc.wantAttached)
		}
	}
}

func TestWalkTmuxPaneReturnsMetadata(t *testing.T) {
	selfAttached := 2
	parentAttached := 0
	panes := map[int]tmuxPaneInfo{
		42: {Location: "self:0.0", Attached: &selfAttached},
		7:  {Location: "parent:1.3", Attached: &parentAttached},
	}
	ppid := map[int]int{99: 7}

	tests := []struct {
		name         string
		pid          int
		wantFound    bool
		wantLocation string
		wantAttached int
	}{
		{"pid itself", 42, true, "self:0.0", 2},
		{"ancestor", 99, true, "parent:1.3", 0},
		{"missing", 123, false, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, found := walkTmuxPane(tc.pid, panes, ppid)
			if found != tc.wantFound {
				t.Fatalf("walkTmuxPane(%d) found = %v, want %v", tc.pid, found, tc.wantFound)
			}
			if !found {
				return
			}
			if info.Location != tc.wantLocation {
				t.Errorf("walkTmuxPane(%d) location = %q, want %q", tc.pid, info.Location, tc.wantLocation)
			}
			if info.Attached == nil || *info.Attached != tc.wantAttached {
				t.Errorf("walkTmuxPane(%d) attached = %v, want %d", tc.pid, info.Attached, tc.wantAttached)
			}
		})
	}
}

func intPtrForTmuxTest(n int) *int { return &n }
