package main

import (
	"context"
	"time"
)

// infoDialogTimeout bounds how long the caller waits to join a fetch — an
// in-flight one or a cache-served one — not the underlying subprocess/fetch
// pipeline's own lifetime: both the ticket and conversation pipelines route
// through a summaryCache.getOrFetch (info_cache.go), which runs the actual
// fetch under a context the cache itself owns, bounded by that cache's own
// fetchTimeout (ticketCacheFetchTimeout / conversationCacheFetchTimeout, see
// info_ticket.go / info_transcript.go) — never this ctx. Keep
// infoDialogTimeout >= the largest of those fetchTimeouts: if the caller
// gives up before the fetch it's joined on could possibly finish, that's a
// caller-side timeout wired shorter than the thing it's waiting on, not a
// safety bound. (See info_exec.go's subprocessWaitDelay doc comment for why
// a subprocess close() alone isn't a hard guarantee of termination.)
const infoDialogTimeout = 20 * time.Second

// asyncSection wraps previewPane (kill_preview.go) — reused for its
// wake-pipe + snapshot mechanics, not its rendering (previewBlock/
// renderConfirmOverlay are single-block and preview-specific; the info
// dialog's own renderer is new, see info_dialog.go) — with an owned
// context.CancelFunc. That CancelFunc bounds how long the caller
// (asyncSection) waits for a result; it does NOT reach into or cancel the
// underlying subprocess — that lifetime is owned by whichever
// summaryCache's own fetchTimeout the pipeline routes through (see
// info_cache.go and infoDialogTimeout's doc comment). close() is idempotent
// and nil-receiver-safe, matching previewPane's own contract, so callers
// never need to nil-check before calling it.
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
