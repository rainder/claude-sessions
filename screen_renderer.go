package main

import (
	"fmt"
	"io"
	"strings"
)

const (
	screenSyncBegin = "\x1b[?2026h"
	screenSyncEnd   = "\x1b[?2026l"
	screenHome      = "\x1b[H"
	screenEraseLine = "\x1b[K"
)

type screenRenderer struct {
	w        io.Writer
	previous []string
	cols     int
	rows     int
	valid    bool
}

func newScreenRenderer(w io.Writer) *screenRenderer {
	return &screenRenderer{w: w}
}

func (r *screenRenderer) Invalidate() {
	r.valid = false
}

func (r *screenRenderer) Draw(content string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		r.Invalidate()
		return r.write(screenSyncBegin + screenHome + content + ansiReset + screenSyncEnd)
	}

	next := normalizedScreenRows(content, cols, rows)
	full := !r.valid || r.cols != cols || r.rows != rows || len(r.previous) != rows

	var patch strings.Builder
	for i, line := range next {
		if !full && line == r.previous[i] {
			continue
		}
		if patch.Len() == 0 {
			patch.WriteString(screenSyncBegin)
		}
		fmt.Fprintf(&patch, "\x1b[%d;1H", i+1)
		patch.WriteString(line)
		patch.WriteString(ansiReset)
		patch.WriteString(screenEraseLine)
	}
	if patch.Len() == 0 {
		return nil
	}
	patch.WriteString(screenSyncEnd)
	if err := r.write(patch.String()); err != nil {
		return err
	}

	r.previous = append(r.previous[:0], next...)
	r.cols = cols
	r.rows = rows
	r.valid = true
	return nil
}

func normalizedScreenRows(content string, cols, rows int) []string {
	input := strings.Split(content, "\n")
	out := make([]string, rows)
	if len(input) > rows {
		input = input[:rows]
	}
	for i, line := range input {
		out[i] = clipLine(line, cols)
	}
	return out
}

func (r *screenRenderer) write(payload string) error {
	n, err := r.w.Write([]byte(payload))
	if err != nil {
		r.Invalidate()
		return err
	}
	if n != len(payload) {
		r.Invalidate()
		return io.ErrShortWrite
	}
	return nil
}
