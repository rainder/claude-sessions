package main

import (
	"context"
	"strings"
	"testing"
)

func TestInfoDialogHeaderUsesUpdatedAtWhenPresent(t *testing.T) {
	s := Session{Name: "my-session", CWD: "/tmp/nogit", StartedAt: 1000, UpdatedAt: 2000}
	lines := infoDialogHeader(s)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "my-session") {
		t.Errorf("header missing name: %v", lines)
	}
	if !strings.Contains(joined, "updated:") {
		t.Errorf("header missing 'updated:' label: %v", lines)
	}
}

func TestInfoDialogHeaderFallsBackToStartedAt(t *testing.T) {
	s := Session{Name: "s2", CWD: "/tmp/nogit", StartedAt: 1000, UpdatedAt: 0}
	lines := infoDialogHeader(s)
	if !strings.Contains(strings.Join(lines, "\n"), "updated:") {
		t.Errorf("want 'updated:' label even when falling back to StartedAt: %v", lines)
	}
}

func TestInfoDialogHeaderShowsHostOnlyWhenRemote(t *testing.T) {
	local := infoDialogHeader(Session{Name: "s", CWD: "/tmp", Host: ""})
	for _, l := range local {
		if strings.HasPrefix(l, "host:") {
			t.Errorf("local session header should not show a host line: %v", local)
		}
	}
	remote := infoDialogHeader(Session{Name: "s", CWD: "/tmp", Host: "myhost"})
	found := false
	for _, l := range remote {
		if l == "host: myhost" {
			found = true
		}
	}
	if !found {
		t.Errorf("remote session header should show its host: %v", remote)
	}
}

func TestRenderInfoDialogOmitsNilTicketSection(t *testing.T) {
	header := []string{"name", "dir"}
	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "convo summary"}, nil
	})
	defer convoSec.close()
	out := renderInfoDialog(header, nil, convoSec, 80, 24)
	if strings.Contains(out, "ticket") {
		t.Errorf("no ticket id -> no ticket section, got:\n%s", out)
	}
}

func TestRenderInfoDialogShowsLoadingBeforeFetchLands(t *testing.T) {
	header := []string{"name"}
	block := make(chan struct{})
	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		<-block
		return PreviewResult{Content: "done"}, nil
	})
	defer func() { close(block); convoSec.close() }()
	out := renderInfoDialog(header, nil, convoSec, 80, 24)
	if !strings.Contains(out, "loading") {
		t.Errorf("want a loading indicator before the fetch lands, got:\n%s", out)
	}
}
