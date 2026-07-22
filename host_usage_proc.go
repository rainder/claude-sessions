package main

import (
	"strconv"
	"strings"
)

type linuxCPUTimes struct {
	total uint64
	idle  uint64
}

func parseLinuxCPUTimes(data string) (linuxCPUTimes, bool) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		if len(fields) < 9 {
			return linuxCPUTimes{}, false
		}
		values := make([]uint64, 8)
		for i := range values {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return linuxCPUTimes{}, false
			}
			values[i] = v
		}
		var total uint64
		for _, v := range values {
			total += v
		}
		return linuxCPUTimes{total: total, idle: values[3] + values[4]}, true
	}
	return linuxCPUTimes{}, false
}

func linuxCPUPercent(prev, next linuxCPUTimes) *float64 {
	if next.total <= prev.total || next.idle < prev.idle {
		return nil
	}
	totalDelta := next.total - prev.total
	idleDelta := next.idle - prev.idle
	if idleDelta > totalDelta {
		return nil
	}
	return hostPercent(float64(totalDelta-idleDelta) / float64(totalDelta) * 100)
}

func parseLinuxMemory(data string) *float64 {
	var total, available uint64
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if key != "MemTotal" && key != "MemAvailable" {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil
		}
		switch key {
		case "MemTotal":
			total, haveTotal = v, true
		case "MemAvailable":
			available, haveAvailable = v, true
		}
	}
	if !haveTotal || !haveAvailable || total == 0 || available > total {
		return nil
	}
	return hostPercent(float64(total-available) / float64(total) * 100)
}
