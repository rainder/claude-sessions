package main

import (
	"errors"
	"strings"
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
