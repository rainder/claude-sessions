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
	a.cancel()
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

// modalWakesWithAll merges the wake fds of multiple preview panes onto
// wakes, for a modal (like the info dialog) with more than one asyncSection.
// Safe to chain: each call to modalWakesWith allocates a fresh backing
// array (len+1 capacity) before appending, so it never mutates a shared
// slice regardless of which of its two branches ran — see
// confirm_overlay.go:189-201.
func modalWakesWithAll(wakes []wakeFD, panes ...*previewPane) []wakeFD {
	for _, p := range panes {
		wakes = modalWakesWith(wakes, p)
	}
	return wakes
}
