package main

import (
	"strings"
	"testing"
)

// TestAccountRowsFrom proves the union every consumer shares: the snapshots a
// host reported plus its live one, marked active from activeSnapshotName (not
// re-derived from an email match), sorted by name, deduped.
func TestAccountRowsFrom(t *testing.T) {
	tests := []struct {
		name string
		snap accountSnapshot
		want []accountRow
	}{
		{
			name: "live account joins the known ones",
			snap: accountSnapshot{
				Usage:      &AccountUsage{Account: "andy@trecs.aero"},
				Known:      []KnownAccountUsage{{Name: "avisoma", Account: "andy@avisoma.com"}},
				ActiveName: "trecs",
			},
			want: []accountRow{
				{Host: "box", Name: "avisoma", Email: "andy@avisoma.com"},
				{Host: "box", Name: "trecs", Email: "andy@trecs.aero", Active: true},
			},
		},
		{
			name: "an expired known account still gets a row",
			snap: accountSnapshot{
				Usage:      &AccountUsage{Account: "andy@avisoma.com"},
				Known:      []KnownAccountUsage{{Name: "trecs", Account: "andy@trecs.aero", Expired: true}},
				ActiveName: "avisoma",
			},
			want: []accountRow{
				{Host: "box", Name: "avisoma", Email: "andy@avisoma.com", Active: true},
				{Host: "box", Name: "trecs", Email: "andy@trecs.aero"},
			},
		},
		{
			name: "no usage snapshot yet leaves the active email unknown",
			snap: accountSnapshot{ActiveName: "avisoma"},
			want: []accountRow{{Host: "box", Name: "avisoma", Active: true}},
		},
		{
			name: "an older server reports nothing at all",
			snap: accountSnapshot{},
			want: nil,
		},
		{
			name: "the live account named in both places yields one row",
			snap: accountSnapshot{
				Usage:      &AccountUsage{Account: "andy@avisoma.com"},
				Known:      []KnownAccountUsage{{Name: "avisoma", Account: "andy@avisoma.com"}},
				ActiveName: "avisoma",
			},
			want: []accountRow{{Host: "box", Name: "avisoma", Email: "andy@avisoma.com", Active: true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := accountRowsFrom("box", tt.snap)
			if len(got) != len(tt.want) {
				t.Fatalf("rows = %+v, want %+v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("row %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestLocalAccountListing proves the local half reads snapshot emails and marks
// the one matching ~/.claude.json's live email.
func TestLocalAccountListing(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-a", "andy@avisoma.com")
	f.snapshot("trecs", "tok-t", "andy@trecs.aero")
	f.setIdentity("andy@trecs.aero")

	got := localAccountListing()
	if got.Error != "" {
		t.Fatalf("error = %q", got.Error)
	}
	want := []accountRow{
		{Host: "local", Name: "avisoma", Email: "andy@avisoma.com"},
		{Host: "local", Name: "trecs", Email: "andy@trecs.aero", Active: true},
	}
	if len(got.Rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got.Rows, want)
	}
	for i := range want {
		if got.Rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got.Rows[i], want[i])
		}
	}
}

// TestRenderAccountTable proves the table layout, and that one unreachable host
// costs exactly one line rather than the whole command.
func TestRenderAccountTable(t *testing.T) {
	out := renderAccountTable([]accountListing{
		{Host: "local", Rows: []accountRow{
			{Host: "local", Name: "avisoma", Email: "andy@avisoma.com", Active: true},
			{Host: "local", Name: "trecs", Email: "andy@trecs.aero"},
		}},
		{Host: "agent-workstation", Error: "connection refused"},
		{Host: "pi", Rows: nil},
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %d, want header + 2 local + 1 error + 1 empty:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "HOST") || !strings.Contains(lines[0], "ACTIVE") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "yes") || !strings.Contains(lines[1], "avisoma") {
		t.Fatalf("active row = %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], "no") {
		t.Fatalf("inactive row = %q", lines[2])
	}
	if !strings.Contains(lines[3], "agent-workstation") || !strings.Contains(lines[3], "connection refused") {
		t.Fatalf("error row = %q", lines[3])
	}
	if !strings.Contains(lines[4], "no account snapshots") {
		t.Fatalf("empty row = %q", lines[4])
	}
	// Columns are padded to the widest host, so every row starts its NAME column
	// at the same offset.
	if strings.Index(lines[1], "avisoma") != strings.Index(lines[0], "NAME") {
		t.Fatalf("columns are not aligned:\n%s", out)
	}
}

// TestRemoteAccountListingUnreachable proves a failed poll becomes a listing
// error rather than an empty (and misleading) row set.
func TestRemoteAccountListingUnreachable(t *testing.T) {
	got := remoteAccountListing(RemoteResult{Name: "box", Error: "HTTP 500"})
	if got.Error != "HTTP 500" || len(got.Rows) != 0 {
		t.Fatalf("listing = %+v, want the error and no rows", got)
	}
}
