package main

import (
	"context"
	"time"
)

// infoDialogTimeout bounds a single fetch pipeline (cu+claude, or
// transcript-read/fetch+claude) even if the dialog is never closed —
// e.g. a wedged subprocess whose pipe close() can't force shut immediately
// (see info_exec.go's subprocessWaitDelay doc comment for why close() alone
// isn't a hard guarantee).
const infoDialogTimeout = 20 * time.Second

// asyncSection wraps previewPane (kill_preview.go) — reused for its
// wake-pipe + snapshot mechanics, not its rendering (previewBlock/
// renderConfirmOverlay are single-block and preview-specific; the info
// dialog's own renderer is new, see info_dialog.go) — with an owned
// context.CancelFunc, so closing the dialog actually signals the
// underlying subprocess(es) to stop instead of just tearing down the wake
// pipe. close() is idempotent and nil-receiver-safe, matching previewPane's
// own contract, so callers never need to nil-check before calling it.
type asyncSection struct {
	*previewPane
	cancel context.CancelFunc
}

func startAsyncSection(title string, run func(ctx context.Context) (PreviewResult, error)) *asyncSection {
	ctx, cancel := context.WithTimeout(context.Background(), infoDialogTimeout)
	pane := startPreviewPane(title, func() (PreviewResult, error) { return run(ctx) })
	return &asyncSection{previewPane: pane, cancel: cancel}
}

func (a *asyncSection) close() {
	if a == nil {
		return
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.previewPane.close()
}

// snapshot shadows previewPane's promoted method so a nil *asyncSection is
// safe to call directly: the promoted method would otherwise dereference a
// nil asyncSection to read the embedded *previewPane field before that
// method's own nil check ever runs.
func (a *asyncSection) snapshot() overlayPreview {
	if a == nil {
		return overlayPreview{}
	}
	return a.previewPane.snapshot()
}

// pane returns a's underlying previewPane, or nil if a itself is nil — lets
// callers pass asyncSection values directly to modalWakesWithAll without a
// nil check, the same nil-receiver-safe contract close()/snapshot() already
// have.
func (a *asyncSection) pane() *previewPane {
	if a == nil {
		return nil
	}
	return a.previewPane
}

// modalWakesWithAll merges the wake fds of multiple asyncSections onto
// wakes, for a modal (like the info dialog) with more than one
// asyncSection — any of which may be nil (e.g. no ticket id detected), so
// this takes *asyncSection rather than the underlying *previewPane and
// resolves each via the nil-safe pane() before merging.
// modalWakesWith never mutates its input: one branch returns it aliased and
// unwritten, the other returns a freshly allocated slice with no spare
// capacity, so neither leaves room for a later append to corrupt anything
// else holding the original slice — see confirm_overlay.go:189-201.
func modalWakesWithAll(wakes []wakeFD, sections ...*asyncSection) []wakeFD {
	for _, sec := range sections {
		wakes = modalWakesWith(wakes, sec.pane())
	}
	return wakes
}
