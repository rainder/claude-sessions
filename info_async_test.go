package main

import (
	"context"
	"testing"
	"time"
)

func TestAsyncSectionDeliversResult(t *testing.T) {
	sec := startAsyncSection("t", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "done"}, nil
	})
	defer sec.close()
	deadline := time.After(time.Second)
	for {
		snap := sec.snapshot()
		if snap.Loaded {
			// startPreviewPane splits PreviewResult.Content into
			// overlayPreview.Lines (kill_preview.go:141-143,
			// strings.Split(res.Content, "\n")) — a one-line result is
			// Lines == []string{"done"}.
			if len(snap.Lines) == 0 || snap.Lines[0] != "done" {
				t.Errorf("Lines = %+v, want [\"done\"]", snap.Lines)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for fetch to land")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAsyncSectionCloseCancelsContext(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	sec := startAsyncSection("t", func(ctx context.Context) (PreviewResult, error) {
		ctxCh <- ctx
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	ctx := <-ctxCh
	sec.close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not canceled by close()")
	}
}

func TestAsyncSectionNilIsSafe(t *testing.T) {
	var sec *asyncSection
	sec.close() // must not panic
	snap := sec.snapshot()
	if snap.Loaded {
		t.Error("nil section snapshot should not report Loaded")
	}
}

func TestModalWakesWithAllChaining(t *testing.T) {
	sec1 := startAsyncSection("a", func(ctx context.Context) (PreviewResult, error) {
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	sec2 := startAsyncSection("b", func(ctx context.Context) (PreviewResult, error) {
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	defer sec1.close()
	defer sec2.close()
	base := []wakeFD{{fd: -1, kind: wakeResize}}
	merged := modalWakesWithAll(base, sec1.previewPane, sec2.previewPane)
	if len(merged) != 3 {
		t.Fatalf("got %d wake fds, want 3 (1 base + 2 panes)", len(merged))
	}
	if len(base) != 1 {
		t.Errorf("modalWakesWithAll must not mutate the caller's base slice, got len %d", len(base))
	}
}
