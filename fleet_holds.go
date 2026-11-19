package main

import (
	"encoding/json"
	"strings"
	"unicode"
)

// Model families Andy put on hold. fleet-state.py stamps state.json's
// top-level "holds" on every publish: model family -> a short reason
// ("Fable": "no new spawns; ..."). An empty object means no hold. The page's
// capacity strip and the TUI's hold line show a held family as HOLD.

const fleetHoldReasonMax = 200 // runes

// fleetHolds is state.json's holds, cleaned: the family name follows the caps
// rule (fleetCapKeyRe), the reason must be a string, is folded to one line
// (control characters dropped, whitespace runs collapsed, trimmed) and cut
// to fleetHoldReasonMax runes. An entry with a bad name, a value that is not
// a string, or an empty reason is dropped. nil when holds is missing, not an
// object, or has no valid entry.
func fleetHolds(state map[string]json.RawMessage) map[string]string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(state["holds"], &raw) != nil || len(raw) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		var s string
		if !fleetCapKeyRe.MatchString(k) || json.Unmarshal(v, &s) != nil {
			continue
		}
		if s = fleetHoldReason(s); s != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fleetHoldReason folds a hold reason to one clean line of at most
// fleetHoldReasonMax runes.
func fleetHoldReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if rs := []rune(s); len(rs) > fleetHoldReasonMax {
		s = strings.TrimSpace(string(rs[:fleetHoldReasonMax]))
	}
	return s
}
