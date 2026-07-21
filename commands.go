package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Scriptable subcommands. Used by the HTTP server (shell-out) and available
// from the shell for ad-hoc automation. All non-interactive.

func cmdKill(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions kill PID [-y]")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "kill: not a pid: %s\n", args[0])
		return 2
	}
	assumeYes := len(args) > 1 && args[1] == "-y"
	if _, ok := readSessionByPID(pid); !ok {
		fmt.Fprintf(os.Stderr, "PID %d is not a live Claude session\n", pid)
		return 1
	}
	if !assumeYes {
		if !confirm(fmt.Sprintf("kill PID %d? [y/N] ", pid)) {
			fmt.Println("aborted")
			return 0
		}
	}
	if err := KillSession(pid); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("killed PID %d\n", pid)
	return 0
}

func cmdMigrate(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions migrate PID [-y]")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: not a pid: %s\n", args[0])
		return 2
	}
	assumeYes := len(args) > 1 && args[1] == "-y"
	sess, ok := readSessionByPID(pid)
	if !ok {
		fmt.Fprintf(os.Stderr, "PID %d is not a live Claude session\n", pid)
		return 1
	}
	tname := MakeTmuxName(sess.CWD, sess.SessionID, sess.Name)
	if !assumeYes {
		if !confirm(fmt.Sprintf("migrate PID %d to tmux %q? [y/N] ", pid, tname)) {
			fmt.Println("aborted")
			return 0
		}
	}
	out, err := MigrateLocal(pid)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(out)
	return 0
}

func cmdNew(args []string) int {
	var cwd, name string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cwd":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "new: --cwd needs a value")
				return 2
			}
			cwd = args[i+1]
			i++
		case "--name":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "new: --name needs a value")
				return 2
			}
			name = args[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "new: unknown arg %q\n", args[i])
			return 2
		}
	}
	if cwd == "" {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions new --cwd PATH [--name NAME]")
		return 2
	}
	cwd = expandTilde(cwd)
	if !isDir(cwd) {
		fmt.Fprintf(os.Stderr, "not a directory: %s\n", cwd)
		return 1
	}
	presets, err := LoadCommandPresets()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tname, err := SpawnNew(cwd, name, presets[0].Command)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(tname)
	return 0
}

func cmdPreview(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions preview PID")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: not a pid: %s\n", args[0])
		return 2
	}
	fmt.Print(PreviewContent(pid))
	return 0
}

func cmdTmuxInfo(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions tmux-info PID")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	loc := tmuxSessionForPID(pid)
	if loc != "" {
		fmt.Println(loc)
	}
	return 0
}

// cmdAttach mirrors the bash behavior: attach to tmux if pid is in a pane,
// else offer migration. Non-interactive callers (e.g. the server) should use
// migrate + tmux-info directly.
func cmdAttach(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions attach PID")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	if _, ok := readSessionByPID(pid); !ok {
		fmt.Fprintf(os.Stderr, "PID %d is not a live Claude session\n", pid)
		return 1
	}
	sessName := tmuxSessionForPID(pid)
	if sessName == "" {
		fmt.Fprintf(os.Stderr, "PID %d is not in tmux\n", pid)
		fmt.Fprintln(os.Stderr, "run: claude-sessions migrate", pid)
		return 1
	}
	subcommand := "attach"
	if os.Getenv("TMUX") != "" {
		subcommand = "switch-client"
	}
	cmd := exec.Command("tmux", subcommand, "-t", sessName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tmux %s: %v\n", subcommand, err)
		return 1
	}
	return 0
}

// trimColon returns the part before the first colon. Useful when tmux-info
// includes the window/pane suffix (session:1.0) but we only want "session".
func trimColon(s string) string { return strings.SplitN(s, ":", 2)[0] }
