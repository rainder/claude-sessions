package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var answeredNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func attemptAt(key, status string, at time.Time) fleetAttempt {
	return fleetAttempt{OptionKey: key, Label: key, At: at.UTC().Format(time.RFC3339), Status: status}
}

func decisionMap(hashes map[string]string) map[string]fleetDecisionCard {
	out := map[string]fleetDecisionCard{}
	for id, h := range hashes {
		out[id] = fleetDecisionCard{Hash: h, Card: fleetCardView{ID: id, Kind: "needs-you"}}
	}
	return out
}

func TestFleetAnsweredRecordKeepsLastThree(t *testing.T) {
	st := &fleetAnsweredStore{}
	for _, k := range []string{"1", "2", "3", "4"} {
		st.record("c", "h"+k, attemptAt(k, fleetAttemptSent, answeredNow), nil, answeredNow)
	}
	e, ok := st.get("c")
	if !ok || e.CardHash != "h4" || e.CardChanged || len(e.Attempts) != 3 {
		t.Fatalf("got %+v", e)
	}
	for i, k := range []string{"2", "3", "4"} {
		if e.Attempts[i].OptionKey != k {
			t.Fatalf("attempt %d = %s", i, e.Attempts[i].OptionKey)
		}
	}
	if e.latest().OptionKey != "4" {
		t.Fatal("latest")
	}
	if (fleetAnswer{}).latest() != (fleetAttempt{}) {
		t.Fatal("empty latest must be zero")
	}
}

func TestFleetAnsweredPrune(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		at          time.Time
		cards       map[string]fleetDecisionCard
		wantKept    bool
		wantChanged bool
	}{
		{"card missing", fleetAttemptSent, answeredNow, decisionMap(map[string]string{}), false, false},
		{"other card only", fleetAttemptUnconfirmed, answeredNow, decisionMap(map[string]string{"x": "h"}), false, false},
		{"hash differs sent", fleetAttemptSent, answeredNow, decisionMap(map[string]string{"c": "new"}), false, false},
		{"hash differs unconfirmed", fleetAttemptUnconfirmed, answeredNow, decisionMap(map[string]string{"c": "new"}), true, true},
		{"hash equal", fleetAttemptSent, answeredNow, decisionMap(map[string]string{"c": "h"}), true, false},
		{"expired", fleetAttemptSent, answeredNow.Add(-24*time.Hour - time.Second), decisionMap(map[string]string{"c": "h"}), false, false},
		{"not yet expired", fleetAttemptSent, answeredNow.Add(-24*time.Hour + time.Second), decisionMap(map[string]string{"c": "h"}), true, false},
		{"nil cards", fleetAttemptUnconfirmed, answeredNow, nil, true, false},
		{"nil cards old", fleetAttemptSent, answeredNow.Add(-48 * time.Hour), nil, true, false},
	}
	for _, tc := range cases {
		st := &fleetAnsweredStore{loaded: true, entries: map[string]fleetAnswer{
			"c": {CardHash: "h", Attempts: []fleetAttempt{attemptAt("1", tc.status, tc.at)}},
		}}
		v := st.view(tc.cards, answeredNow)
		e, ok := v["c"]
		if ok != tc.wantKept || (ok && e.CardChanged != tc.wantChanged) {
			t.Errorf("%s: kept=%v changed=%v", tc.name, ok, e.CardChanged)
		}
	}
	// A flagged entry comes back to unflagged when the hash is equal again.
	st := &fleetAnsweredStore{loaded: true, entries: map[string]fleetAnswer{
		"c": {CardHash: "h", CardChanged: true, Attempts: []fleetAttempt{attemptAt("1", fleetAttemptUnconfirmed, answeredNow)}},
	}}
	if st.view(decisionMap(map[string]string{"c": "h"}), answeredNow)["c"].CardChanged {
		t.Fatal("flag must clear when the hash matches")
	}
}

func TestFleetAnswerRefused(t *testing.T) {
	mk := func(status string) fleetAnswer {
		return fleetAnswer{CardHash: "h", Attempts: []fleetAttempt{attemptAt("1", status, answeredNow)}}
	}
	cases := []struct {
		name       string
		e          fleetAnswer
		has        bool
		cur        string
		correction bool
		want       bool
	}{
		{"no entry", fleetAnswer{}, false, "h", false, false},
		{"sent same", mk(fleetAttemptSent), true, "h", false, true},
		{"sent other", mk(fleetAttemptSent), true, "z", false, false},
		{"unconfirmed same", mk(fleetAttemptUnconfirmed), true, "h", false, true},
		{"unconfirmed other", mk(fleetAttemptUnconfirmed), true, "z", false, true},
		{"correction sent", mk(fleetAttemptSent), true, "h", true, false},
		{"correction unconfirmed", mk(fleetAttemptUnconfirmed), true, "h", true, false},
	}
	for _, tc := range cases {
		if got := fleetAnswerRefused(tc.e, tc.has, tc.cur, tc.correction); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestFleetAnsweredDismiss(t *testing.T) {
	st := &fleetAnsweredStore{}
	if st.dismiss("c") {
		t.Fatal("unknown id")
	}
	st.record("c", "h", attemptAt("1", fleetAttemptSent, answeredNow), nil, answeredNow)
	if !st.dismiss("c") {
		t.Fatal("known id")
	}
	if _, ok := st.get("c"); ok {
		t.Fatal("entry must be gone")
	}
}

func TestFleetAnsweredPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "st", "fleet-answered.json")
	st := &fleetAnsweredStore{path: path}
	st.record("c", "h", attemptAt("1", fleetAttemptSent, answeredNow), nil, answeredNow)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	st2 := &fleetAnsweredStore{path: path}
	if !reflect.DeepEqual(st.view(nil, answeredNow), st2.view(nil, answeredNow)) {
		t.Fatalf("reload differs: %+v vs %+v", st.view(nil, answeredNow), st2.view(nil, answeredNow))
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := (&fleetAnsweredStore{path: bad}).view(nil, answeredNow); v == nil || len(v) != 0 {
		t.Fatalf("bad file: %+v", v)
	}

	at := answeredNow.Format(time.RFC3339)
	good := func(k string) fleetAttempt { return fleetAttempt{OptionKey: k, At: at, Status: fleetAttemptSent} }
	file := map[string]any{"answered": map[string]fleetAnswer{
		"badstatus": {CardHash: "h", Attempts: []fleetAttempt{{OptionKey: "1", At: at, Status: "weird"}}},
		"noattempt": {CardHash: "h"},
		"nohash":    {Attempts: []fleetAttempt{good("1")}},
		"badat":     {CardHash: "h", Attempts: []fleetAttempt{{OptionKey: "1", At: "soon", Status: fleetAttemptSent}}},
		"ok":        {CardHash: "h", Attempts: []fleetAttempt{good("1"), good("2"), good("3"), good("4")}},
	}}
	data, _ := json.Marshal(file)
	mixed := filepath.Join(t.TempDir(), "mixed.json")
	if err := os.WriteFile(mixed, data, 0o600); err != nil {
		t.Fatal(err)
	}
	v := (&fleetAnsweredStore{path: mixed}).view(nil, answeredNow)
	if len(v) != 1 || len(v["ok"].Attempts) != 3 || v["ok"].Attempts[0].OptionKey != "2" {
		t.Fatalf("mixed: %+v", v)
	}
}

func TestFleetAnsweredWritesOnlyOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-answered.json")
	st := &fleetAnsweredStore{path: path}
	st.view(decisionMap(nil), answeredNow)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("view on an empty store must not create the file")
	}
	st.record("c", "h", attemptAt("1", fleetAttemptSent, answeredNow), nil, answeredNow)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	st.view(decisionMap(map[string]string{"c": "h"}), answeredNow)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().After(old.Add(time.Minute)) {
		t.Fatal("unchanged view rewrote the file")
	}
}
