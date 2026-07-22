package main

import "math"

// HostUsage is one whole-host resource snapshot. Nil fields mean unavailable;
// pointers preserve a valid zero percentage through JSON omitempty.
type HostUsage struct {
	CPUPercent    *float64 `json:"cpuPercent,omitempty"`
	MemoryPercent *float64 `json:"memoryPercent,omitempty"`
}

// LocalHost groups the current machine's identity, sessions, and resource
// snapshot for rendering. RemoteResult is the corresponding remote shape.
type LocalHost struct {
	Name      string
	Sessions  []Session
	HostUsage HostUsage
}

func hostPercent(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	v = max(0, min(100, v))
	return &v
}
