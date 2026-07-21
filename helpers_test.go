package main

import (
	"strings"
	"testing"
)

func TestWriteMouseMode(t *testing.T) {
	var b strings.Builder
	writeMouseMode(&b, true)
	writeMouseMode(&b, false)
	want := "\x1b[?1000h\x1b[?1006h\x1b[?1006l\x1b[?1000l"
	if b.String() != want {
		t.Fatalf("mouse sequences = %q, want %q", b.String(), want)
	}
}
