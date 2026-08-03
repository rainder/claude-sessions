package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestInfoDialogHeaderUsesUpdatedAtWhenPresent(t *testing.T) {
	started := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	updated := time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC).UnixMilli()
	s := Session{Name: "my-session", CWD: "/tmp/nogit", StartedAt: started, UpdatedAt: updated}
	lines := infoDialogHeader(s)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "my-session") {
		t.Errorf("header missing name: %v", lines)
	}
	wantUpdated := time.UnixMilli(updated).Format("2006-01-02 15:04")
	wantStarted := time.UnixMilli(started).Format("2006-01-02 15:04")
	if !strings.Contains(joined, "updated: "+wantUpdated) {
		t.Errorf("header should use UpdatedAt (%s), got: %v", wantUpdated, lines)
	}
	if strings.Contains(joined, "updated: "+wantStarted) {
		t.Errorf("header should not use StartedAt (%s) when UpdatedAt is present, got: %v", wantStarted, lines)
	}
}

func TestInfoDialogHeaderFallsBackToStartedAt(t *testing.T) {
	started := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	s := Session{Name: "s2", CWD: "/tmp/nogit", StartedAt: started, UpdatedAt: 0}
	lines := infoDialogHeader(s)
	joined := strings.Join(lines, "\n")
	want := time.UnixMilli(started).Format("2006-01-02 15:04")
	if !strings.Contains(joined, "updated: "+want) {
		t.Errorf("header should fall back to StartedAt (%s), got: %v", want, lines)
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

func TestRenderInfoDialogHasOneDividerPerSection(t *testing.T) {
	header := []string{"name", "dir"}
	ticketSec := startAsyncSection("ticket", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "ticket body"}, nil
	})
	defer ticketSec.close()
	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "convo summary"}, nil
	})
	defer convoSec.close()

	// Block until both sections have landed so the divider count isn't racy
	// against the loading placeholder (which also renders correctly, but
	// this test cares about the settled/loaded shape).
	for i := 0; i < 1000; i++ {
		t1, t2 := ticketSec.snapshot().Loaded, convoSec.snapshot().Loaded
		if t1 && t2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	out := renderInfoDialog(header, ticketSec, convoSec, 80, 40)
	// Match the divider as a full padded row, not a bare substring: the box's
	// top/bottom border lines are also runs of confirmBoxH (just innerWidth+2
	// long) and would otherwise contain the shorter divider as a substring.
	dividerRow := confirmBoxV + " " + strings.Repeat(confirmBoxH, infoDialogInnerWidth) + " " + confirmBoxV
	got := 0
	for _, line := range strings.Split(out, "\n") {
		if line == dividerRow {
			got++
		}
	}
	if got != 2 {
		t.Errorf("want exactly 2 dividers (one before ticket, one before conversation), got %d:\n%s", got, out)
	}
}

func TestRenderInfoDialogShowsSectionErr(t *testing.T) {
	header := []string{"name"}
	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{}, errFakeFetch
	})
	defer convoSec.close()

	for i := 0; i < 1000; i++ {
		if convoSec.snapshot().Loaded {
			break
		}
		time.Sleep(time.Millisecond)
	}

	out := renderInfoDialog(header, nil, convoSec, 80, 24)
	if !strings.Contains(out, "unavailable: "+errFakeFetch.Error()) {
		t.Errorf("want the section's error reflected in output, got:\n%s", out)
	}
}

func TestRenderInfoDialogWrapsLongContent(t *testing.T) {
	header := []string{"name"}
	shortLine := "short"
	longLine := strings.Repeat("word ", 60) // far wider than any reasonable inner width

	convoSecShort := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: shortLine}, nil
	})
	defer convoSecShort.close()
	convoSecLong := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: longLine}, nil
	})
	defer convoSecLong.close()

	for i := 0; i < 1000; i++ {
		t1, t2 := convoSecShort.snapshot().Loaded, convoSecLong.snapshot().Loaded
		if t1 && t2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	shortOut := renderInfoDialog(header, nil, convoSecShort, 80, 40)
	longOut := renderInfoDialog(header, nil, convoSecLong, 80, 40)
	shortLines := strings.Count(shortOut, "\n")
	longLines := strings.Count(longOut, "\n")
	if longLines <= shortLines {
		t.Errorf("want long content wrapped across more output lines than short content, got long=%d short=%d:\nlong:\n%s\nshort:\n%s", longLines, shortLines, longOut, shortOut)
	}
	if !strings.Contains(longOut, "word word") {
		t.Errorf("want wrapped content still present, got:\n%s", longOut)
	}
}

var errFakeFetch = errFake("fake fetch failure")

type errFake string

func (e errFake) Error() string { return string(e) }
