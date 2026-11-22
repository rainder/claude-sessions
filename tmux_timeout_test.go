package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A wedged tmux server must not hang a collect: tmuxPaneMap gives up after
// collectCmdTimeout and returns the empty best-effort map. The fake tmux also
// leaves a background child holding stdout, which is what WaitDelay is for.
func TestTmuxPaneMapTimesOutOnHungTmux(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 30 &\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := collectCmdTimeout
	collectCmdTimeout = 200 * time.Millisecond
	t.Cleanup(func() { collectCmdTimeout = old })

	start := time.Now()
	panes, err := tmuxPaneMap()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("tmuxPaneMap error = %v, want nil (best effort)", err)
	}
	if len(panes) != 0 {
		t.Fatalf("tmuxPaneMap = %v, want empty", panes)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("tmuxPaneMap took %v on a hung tmux, want about the timeout", elapsed)
	}
}
