package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// homeDiskPercent is the seam the collectors call for the DISK bar: the used
// share of the filesystem that holds the home directory. One statfs call, no
// subprocess, so it is cheap at hostUsageInterval.
var homeDiskPercent = func() *float64 {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var st unix.Statfs_t
	if err := unix.Statfs(home, &st); err != nil {
		return nil
	}
	return diskUsedPercent(uint64(st.Blocks), uint64(st.Bfree), uint64(st.Bavail))
}

// diskUsedPercent computes used space the way df does: used / (used + space
// an unprivileged user can still write). Blocks reserved for root count as
// neither, so a disk df shows at 100% also reads 100% here.
func diskUsedPercent(blocks, free, avail uint64) *float64 {
	if blocks == 0 || free > blocks {
		return nil
	}
	used := blocks - free
	if used+avail == 0 {
		return nil
	}
	return hostPercent(float64(used) / float64(used+avail) * 100)
}
