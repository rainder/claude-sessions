package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// usageBucket is one rate-limit window from the OAuth usage endpoint.
type usageBucket struct {
	Pct      float64
	ResetsAt time.Time
}

// UsageInfo is the parsed account rate-limit snapshot shown in the header.
type UsageInfo struct {
	FiveHour usageBucket
	SevenDay usageBucket
}

// parseUsage decodes the /api/oauth/usage response body. Only the overall
// five_hour and seven_day buckets are kept; per-model buckets are ignored
// by design (see the spec).
func parseUsage(body []byte) (*UsageInfo, error) {
	type bucket struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	}
	var raw struct {
		FiveHour *bucket `json:"five_hour"`
		SevenDay *bucket `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.FiveHour == nil || raw.SevenDay == nil {
		return nil, fmt.Errorf("usage response missing five_hour/seven_day")
	}
	return &UsageInfo{
		FiveHour: usageBucket{Pct: raw.FiveHour.Utilization, ResetsAt: raw.FiveHour.ResetsAt},
		SevenDay: usageBucket{Pct: raw.SevenDay.Utilization, ResetsAt: raw.SevenDay.ResetsAt},
	}, nil
}
