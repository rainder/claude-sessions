package main

import (
	"encoding/json"
	"regexp"
)

// The FLEET page's capacity strip. fleet-state.py stamps state.json's
// top-level "caps" on every publish: model family -> the most BUSY sessions
// FLEET runs at once for that family ("Fable", "Opus", "Grok"; Sonnet counts
// as Opus). The server serves only the valid entries; the page shows a
// family with no cap as "<n> busy", never as a cap of 0.

const fleetCapValueMax = 1000 // sessions

var fleetCapKeyRe = regexp.MustCompile(`^[A-Za-z0-9 ._+-]{1,40}$`)

// fleetCaps is state.json's caps, cleaned: an entry with a bad family name
// or a value that is not a positive integer is dropped. nil when caps is
// missing, not an object, or has no valid entry.
func fleetCaps(state map[string]json.RawMessage) map[string]int {
	var raw map[string]json.RawMessage
	if json.Unmarshal(state["caps"], &raw) != nil || len(raw) == 0 {
		return nil
	}
	out := map[string]int{}
	for k, v := range raw {
		var n int
		if !fleetCapKeyRe.MatchString(k) || json.Unmarshal(v, &n) != nil || n < 1 || n > fleetCapValueMax {
			continue
		}
		out[k] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
