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
	merged := modalWakesWithAll(base, sec1, sec2)
	if len(merged) != 3 {
		t.Fatalf("got %d wake fds, want 3 (1 base + 2 panes)", len(merged))
	}
	if len(base) != 1 {
		t.Errorf("modalWakesWithAll must not mutate the caller's base slice, got len %d", len(base))
	}
}

// TestModalWakesWithAllNilSection exercises the case Task 11 depends on:
// a nil *asyncSection (e.g. no ticket id detected) must not panic when
// passed straight into modalWakesWithAll, and must contribute no wake fd of
// its own.
func TestModalWakesWithAllNilSection(t *testing.T) {
	sec := startAsyncSection("a", func(ctx context.Context) (PreviewResult, error) {
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	defer sec.close()
	var nilSec *asyncSection
	base := []wakeFD{{fd: -1, kind: wakeResize}}
	merged := modalWakesWithAll(base, sec, nilSec)
	if len(merged) != 2 {
		t.Fatalf("got %d wake fds, want 2 (1 base + 1 live section; nil section contributes none)", len(merged))
	}
}

// TestModalWakesWithAllAliasingBranch exercises the branch the chaining
// test above doesn't: a closed pane's wake() returns fd: -1, so
// modalWakesWith takes the aliasing return path (unwritten, aliasing the
// input) rather than allocating. Confirms modalWakesWithAll still returns
// the right length/contents through that branch and never mutates base.
func TestModalWakesWithAllAliasingBranch(t *testing.T) {
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
	sec1.close() // sec1's pane wake() now returns fd: -1 -> aliasing branch

	base := []wakeFD{{fd: -1, kind: wakeResize}}
	merged := modalWakesWithAll(base, sec1, sec2)
	if len(merged) != 2 {
		t.Fatalf("got %d wake fds, want 2 (1 base + 1 live section; closed section contributes none)", len(merged))
	}
	if len(base) != 1 {
		t.Errorf("modalWakesWithAll must not mutate the caller's base slice, got len %d", len(base))
	}
}

// TestAsyncSectionCloseIsIdempotent pins that calling close() twice on the
// same section never panics — true by construction (context.CancelFunc is
// idempotent and previewPane.close() guards on p.closed), pinned here as a
// regression test.
func TestAsyncSectionCloseIsIdempotent(t *testing.T) {
	sec := startAsyncSection("a", func(ctx context.Context) (PreviewResult, error) {
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	sec.close()
	sec.close() // must not panic
}
