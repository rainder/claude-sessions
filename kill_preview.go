package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// overlayPreview is an immutable snapshot of a session's recent output, handed
// to renderConfirmOverlay for display inside the kill confirmation box. It is
// deliberately free of tmux/HTTP concerns: previewPane fills it in, the
// renderer only formats it.
type overlayPreview struct {
	Title  string   // "repo:branch · pid 48221" (host-qualified when remote)
	Source string   // "tmux" | "transcript"; empty while loading
	Lines  []string // sanitized pane lines, oldest first, unclipped
	Err    error    // fetch failure; Lines is nil
	Loaded bool     // false while the fetch is still in flight
}

// trimTrailingBlank drops blank lines from the end of a pane capture. tmux
// capture-pane pads its output to the full pane height, so without this the
// preview block renders mostly empty rows.
func trimTrailingBlank(lines []string) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// previewStatusLine returns the dimmed placeholder row to show in place of
// content, or "" when the snapshot has real lines to render.
func previewStatusLine(prev overlayPreview) string {
	switch {
	case !prev.Loaded:
		return dim("loading preview…")
	case errors.Is(prev.Err, errSessionEnded):
		return dim("session already gone")
	case prev.Err != nil:
		return dim("preview unavailable: " + prev.Err.Error())
	case len(trimTrailingBlank(prev.Lines)) == 0:
		return dim("(pane empty)")
	default:
		return ""
	}
}

// previewBlock builds the inner lines of the preview block — title, divider,
// up to contentRows content rows, divider — clipped to innerWidth and
// reset-terminated so an unterminated SGR sequence cannot bleed into the box
// border. The caller supplies the padding and the border itself. Returns nil
// when there is no preview or no room for one.
func previewBlock(prev *overlayPreview, innerWidth, contentRows int) []string {
	if prev == nil || innerWidth < 1 || contentRows < 1 {
		return nil
	}

	body := []string{previewStatusLine(*prev)}
	if body[0] == "" {
		body = trimTrailingBlank(prev.Lines)
		if len(body) > contentRows {
			body = body[len(body)-contentRows:]
		}
	}

	divider := strings.Repeat(confirmBoxH, innerWidth)
	out := make([]string, 0, len(body)+3)
	out = append(out, previewTitleRow(*prev, innerWidth), divider)
	out = append(out, body...)
	out = append(out, divider)

	for i, l := range out {
		out[i] = clipLine(l, innerWidth) + ansiReset
	}
	return out
}

// previewTitleRow renders the identity row: title on the left, dimmed source
// marker flush right. The source is dropped rather than wrapped when the two
// cannot share the row.
func previewTitleRow(prev overlayPreview, innerWidth int) string {
	gap := innerWidth - visualLen(prev.Title) - visualLen(prev.Source)
	if prev.Source == "" || gap < 1 {
		return prev.Title
	}
	return prev.Title + strings.Repeat(" ", gap) + dim(prev.Source)
}

// killPreviewLimits bounds the kill-dialog fetch. At most 12 lines are ever
// rendered; 60 leaves headroom for trailing-blank trimming while keeping the
// remote response body far smaller than DefaultPreviewLimits' 2000 lines.
var killPreviewLimits = PreviewLimits{MaxLines: 60, MaxBytes: 32 << 10}

// previewFetch is the injectable seam around the actual preview lookup, so
// previewPane's lifecycle can be tested without tmux or a remote host.
type previewFetch func() (PreviewResult, error)

// previewPane holds an in-flight preview fetch and the self-pipe the modal
// loop selects on, following RemoteHub's wake-pipe pattern (remote.go:121).
// Every method is nil-safe so callers never have to branch on a nil pane.
type previewPane struct {
	mu     sync.Mutex
	snap   overlayPreview
	wakeR  *os.File
	wakeW  *os.File
	closed bool
}

// startPreviewPane kicks off fetch in the background and returns immediately.
// If the pipe cannot be created the pane still works — it simply never wakes
// the modal, so the placeholder stays until the next keypress redraws.
func startPreviewPane(title string, fetch previewFetch) *previewPane {
	p := &previewPane{snap: overlayPreview{Title: title}}
	if r, w, err := os.Pipe(); err == nil {
		p.wakeR, p.wakeW = r, w
	}
	go func() {
		res, err := fetch()
		p.mu.Lock()
		p.snap.Loaded = true
		if err != nil {
			p.snap.Err = err
		} else {
			p.snap.Source = res.Source
			p.snap.Lines = strings.Split(res.Content, "\n")
		}
		// The write happens under the same lock that close() takes, so the fd
		// can never be closed and reused between the check and the write.
		if !p.closed && p.wakeW != nil {
			_, _ = p.wakeW.Write([]byte{1})
		}
		p.mu.Unlock()
	}()
	return p
}

// snapshot returns the current state by value; the caller may hold it across
// a render without racing the fetch goroutine.
func (p *previewPane) snapshot() overlayPreview {
	if p == nil {
		return overlayPreview{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snap
}

// wake exposes the read end for unix.Select. A negative fd means "no source"
// and is skipped by pollEvents.
func (p *previewPane) wake() wakeFD {
	if p == nil {
		return wakeFD{fd: -1, kind: wakePreview}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.wakeR == nil {
		return wakeFD{fd: -1, kind: wakePreview}
	}
	return wakeFD{fd: int(p.wakeR.Fd()), kind: wakePreview}
}

// close releases the pipe. Idempotent, and safe to call while the fetch is
// still running — the goroutine's write is guarded by the same mutex.
func (p *previewPane) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.wakeR != nil {
		p.wakeR.Close()
	}
	if p.wakeW != nil {
		p.wakeW.Close()
	}
}

// killPreviewTitle is the identity row: the session's display label plus the
// pid, host-qualified for remote rows.
func killPreviewTitle(s Session) string {
	label, _ := s.DisplayName()
	if s.Host != "" {
		return fmt.Sprintf("%s · %s:%d", label, s.Host, s.PID)
	}
	return fmt.Sprintf("%s · pid %d", label, s.PID)
}

// startLocalKillPreview fetches the local pane snapshot for a kill dialog.
func startLocalKillPreview(s Session) *previewPane {
	return startPreviewPane(killPreviewTitle(s), func() (PreviewResult, error) {
		return LoadPreview(s.PID, killPreviewLimits)
	})
}

// startRemoteKillPreview fetches the snapshot over HTTP. The 5s client timeout
// in fetchRemotePreview bounds how long the goroutine can outlive the dialog.
func startRemoteKillPreview(s Session) *previewPane {
	host, pid := s.Host, s.PID
	return startPreviewPane(killPreviewTitle(s), func() (PreviewResult, error) {
		return fetchRemotePreview(host, pid, killPreviewLimits)
	})
}
