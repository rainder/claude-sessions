package main

import (
	"reflect"
	"strings"
	"testing"
)

func targetIDs(targets []selectionTarget) []string {
	ids := make([]string, len(targets))
	for i, target := range targets {
		ids[i] = target.id
	}
	return ids
}

func TestBuildSelectionTargets(t *testing.T) {
	local := []Session{{PID: 10, CWD: "/local"}}
	remotes := []RemoteResult{
		{Name: "beluga"},
		{Name: "loading", Loading: true},
		{Name: "broken", Error: "connection refused"},
		{Name: "orca", Sessions: []Session{{PID: 20, Host: "orca", CWD: "/remote"}}},
		{Name: "narwhal"},
	}

	got := targetIDs(buildSelectionTargets(local, remotes))
	want := []string{
		"10",
		emptyHostSelectionID("beluga"),
		"orca:20",
		emptyHostSelectionID("narwhal"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("target IDs = %q, want %q", got, want)
	}
}

func TestEmptyHostSelectionIDUsesReservedNamespace(t *testing.T) {
	id := emptyHostSelectionID("42")
	if !strings.HasPrefix(id, "\x00host:") {
		t.Fatalf("empty-host ID %q lacks reserved prefix", id)
	}
	if id == "42" || id == "host:42" {
		t.Fatalf("empty-host ID %q can collide with a session ID", id)
	}
}

func TestNavTargetsIncludesEmptyHostsAndWraps(t *testing.T) {
	targets := buildSelectionTargets(
		[]Session{{PID: 10}},
		[]RemoteResult{{Name: "beluga"}, {Name: "narwhal"}},
	)

	beluga := emptyHostSelectionID("beluga")
	narwhal := emptyHostSelectionID("narwhal")
	cases := []struct {
		name  string
		sel   string
		delta int
		want  string
	}{
		{"down enters empty host", "10", 1, beluga},
		{"down enters next empty host", beluga, 1, narwhal},
		{"down wraps", narwhal, 1, "10"},
		{"up wraps", "10", -1, narwhal},
		{"empty selection down", "", 1, "10"},
		{"empty selection up", "", -1, narwhal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := navTargets(targets, tc.sel, tc.delta); got != tc.want {
				t.Fatalf("navTargets(%q, %d) = %q, want %q", tc.sel, tc.delta, got, tc.want)
			}
		})
	}
}

func TestValidateTargetSelFollowsPopulatedHost(t *testing.T) {
	targets := buildSelectionTargets(nil, []RemoteResult{
		{Name: "beluga", Sessions: []Session{
			{PID: 30, Host: "beluga"},
			{PID: 31, Host: "beluga"},
		}},
	})

	got := validateTargetSel(targets, emptyHostSelectionID("beluga"))
	if got != "beluga:30" {
		t.Fatalf("validateTargetSel followed empty host to %q, want %q", got, "beluga:30")
	}
}

func TestValidateTargetSelUsesExistingFallbackForOtherMissingRows(t *testing.T) {
	targets := buildSelectionTargets([]Session{{PID: 10}, {PID: 11}}, nil)
	if got := validateTargetSel(targets, "999"); got != "10" {
		t.Fatalf("validateTargetSel missing session = %q, want first target", got)
	}
	if got := validateTargetSel(nil, "999"); got != "" {
		t.Fatalf("validateTargetSel empty targets = %q, want empty", got)
	}
}
