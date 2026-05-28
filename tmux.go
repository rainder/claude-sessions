package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// tmuxPaneMap returns pane_pid → "session:window.pane" for every tmux pane
// on the default server. Empty map (no error) if tmux isn't running.
func tmuxPaneMap() (map[int]string, error) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_pid} #{session_name}:#{window_index}.#{pane_index}").Output()
	if err != nil {
		return map[int]string{}, nil
	}
	m := make(map[int]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		sp := strings.SplitN(line, " ", 2)
		if len(sp) != 2 {
			continue
		}
		pid, err := strconv.Atoi(sp[0])
		if err != nil {
			continue
		}
		m[pid] = sp[1]
	}
	return m, nil
}

// ppidMap returns pid → ppid for every process on the system.
func ppidMap() (map[int]int, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil, err
	}
	m := make(map[int]int, 256)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pp, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		m[pid] = pp
	}
	return m, nil
}

// processInfo returns pid→ppid and pid→cpu% in a single `ps -A` spawn.
// CollectLocal needs both, so folding them into one call saves N+1 ps
// invocations per tick (one per session for CPU%) down to 1.
func processInfo() (ppid map[int]int, cpu map[int]string, err error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,%cpu=").Output()
	if err != nil {
		return nil, nil, err
	}
	ppid = make(map[int]int, 256)
	cpu = make(map[int]string, 256)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pp, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		ppid[pid] = pp
		cpu[pid] = f[2]
	}
	return ppid, cpu, nil
}

// walkTmuxPane returns the tmux pane string (session:win.pane) if pid is a
// descendant of any tmux pane process, else "". Checks the pid itself first
// since `tmux new-session "claude ..."` makes claude the pane_pid directly.
func walkTmuxPane(pid int, panes map[int]string, ppid map[int]int) string {
	cur := pid
	for i := 0; i < 32; i++ {
		if loc, ok := panes[cur]; ok {
			return loc
		}
		if cur <= 1 {
			return ""
		}
		cur = ppid[cur]
	}
	return ""
}
