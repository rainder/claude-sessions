package main

import (
	"os/exec"
	"strconv"
	"strings"
)

type tmuxPaneInfo struct {
	Location string
	Attached *int
}

// tmuxPaneMap returns pane_pid → pane metadata for every tmux pane on the
// default server. Empty map (no error) if tmux isn't running.
func tmuxPaneMap() (map[int]tmuxPaneInfo, error) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_pid}\t#{session_name}:#{window_index}.#{pane_index}\t#{session_attached}").Output()
	if err != nil {
		return map[int]tmuxPaneInfo{}, nil
	}
	return parseTmuxPaneOutput(string(out)), nil
}

func parseTmuxPaneOutput(out string) map[int]tmuxPaneInfo {
	panes := make(map[int]tmuxPaneInfo)
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[1] == "" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}

		info := tmuxPaneInfo{Location: fields[1]}
		attached, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err == nil && attached >= 0 {
			info.Attached = &attached
		}
		panes[pid] = info
	}
	return panes
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

// walkTmuxPane returns tmux pane metadata if pid is a descendant of any tmux
// pane process. It checks pid itself first because `tmux new-session
// "claude ..."` makes claude the pane_pid directly.
func walkTmuxPane(pid int, panes map[int]tmuxPaneInfo, ppid map[int]int) (tmuxPaneInfo, bool) {
	cur := pid
	for i := 0; i < 32; i++ {
		if info, ok := panes[cur]; ok {
			return info, true
		}
		if cur <= 1 {
			return tmuxPaneInfo{}, false
		}
		cur = ppid[cur]
	}
	return tmuxPaneInfo{}, false
}
