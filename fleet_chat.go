package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const fleetChatTailBytes = 4 << 20

type fleetChatTurn struct {
	UUID string `json:"uuid"`
	Role string `json:"role"`
	At   string `json:"at"`
	Text string `json:"text"`
}

// fleetTurnText decides what one transcript line shows as a turn, and its
// text; ok false skips the line.
type fleetTurnText func(e *transcriptLine) (string, bool)

// fleetChatText is FLEET's own chat: text blocks only, and no user line that
// starts with a wrapper tag (task notifications, command output).
func fleetChatText(e *transcriptLine) (string, bool) {
	text, ok := extractTurnText(e.Message.Content)
	if !ok {
		return "", false
	}
	if e.Type == "user" && strings.HasPrefix(strings.TrimSpace(text), "<") {
		return "", false
	}
	return text, true
}

// fleetTurnRec is one turn plus where it sits in the file: first is the
// offset of its first line (what `older` points at), changedAt of the line
// that last set its text.
type fleetTurnRec struct {
	fleetChatTurn
	first, changedAt int64
}

// scanFleetTurns reads whole lines from rd, starting at file offset off, and
// folds them into turns. A line without '\n' is mid-write and ends the scan.
// end is the offset just past the last whole line read.
func scanFleetTurns(rd *bufio.Reader, off int64, text fleetTurnText) (turns []fleetTurnRec, end int64) {
	idx := map[string]int{}
	end = off
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return turns, end
		}
		lineOff := off
		off += int64(len(line))
		end = off
		var e transcriptLine
		if json.Unmarshal(line, &e) != nil || e.IsMeta {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		t, ok := text(&e)
		if !ok {
			continue
		}
		if e.Type == "assistant" {
			key := e.Message.ID + "\x00" + e.RequestID
			if i, ok := idx[key]; ok {
				turns[i].Text = t
				turns[i].changedAt = lineOff
				continue
			}
			idx[key] = len(turns)
		}
		turns = append(turns, fleetTurnRec{fleetChatTurn{UUID: e.UUID, Role: e.Type, At: e.Timestamp, Text: t}, lineOff, lineOff})
	}
}

// extractChatTurns is extractTurns with FLEET's chat filter.
func extractChatTurns(path string, since int64, n int, tailBytes int64) ([]fleetChatTurn, int64, bool, error) {
	turns, pos, reset, _, err := extractTurns(path, since, n, tailBytes, fleetChatText)
	return turns, pos, reset, err
}

// extractTurns reads the transcript tail and returns the turns that changed
// at or after byte offset since — new turns, and turns whose text a later
// line of the same assistant message replaced — plus pos, the offset for the
// next call. A turn keeps the uuid of its first line, so the page can upsert.
// reset tells the page to replace its list instead of merging. older, on a
// reset answer, is the offset to pass as `before` for the turns ahead of the
// ones returned (0 = none known); an incremental answer leaves it 0.
func extractTurns(path string, since int64, n int, tailBytes int64, text fleetTurnText) ([]fleetChatTurn, int64, bool, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, true, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, true, 0, err
	}
	size := st.Size()
	if since > 0 && since == size {
		return nil, size, false, 0, nil
	}
	start := int64(0)
	if size > tailBytes {
		start = size - tailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, true, 0, err
	}
	rd := bufio.NewReaderSize(f, 64*1024)
	off := start
	if start > 0 { // drop the partial first line
		skipped, err := rd.ReadBytes('\n')
		off += int64(len(skipped))
		if err != nil {
			return nil, off, true, 0, nil
		}
	}
	windowStart := off
	turns, pos := scanFleetTurns(rd, off, text)

	reset := since <= 0 || since > size || since < windowStart
	var out []fleetTurnRec
	for _, t := range turns {
		if reset || t.changedAt >= since {
			out = append(out, t)
		}
	}
	if len(out) > n {
		out, reset = out[len(out)-n:], true
	}
	var older int64
	if reset && len(out) > 0 && (len(out) < len(turns) || windowStart > 0) {
		older = out[0].first
	}
	return plainTurns(out), pos, reset, older, nil
}

// extractTurnsBefore pages backward: up to n turns whose first line starts
// before byte offset before, read in growing chunks, and older for the page
// ahead of them (0 = none). Only whole lines before `before` count.
func extractTurnsBefore(path string, before int64, n int, text fleetTurnText) ([]fleetChatTurn, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if before > st.Size() {
		before = st.Size()
	}
	for chunk := int64(1 << 20); ; chunk *= 2 {
		start := max(0, before-chunk)
		rd := bufio.NewReaderSize(io.NewSectionReader(f, start, before-start), 64*1024)
		off := start
		if start > 0 {
			skipped, err := rd.ReadBytes('\n')
			off += int64(len(skipped))
			if err != nil { // one line longer than the chunk
				if chunk >= fleetLogMaxScan {
					return nil, 0, nil
				}
				continue
			}
		}
		turns, _ := scanFleetTurns(rd, off, text)
		if len(turns) < n && start > 0 && chunk < fleetLogMaxScan {
			continue
		}
		out := turns
		if len(out) > n {
			out = out[len(out)-n:]
		}
		var older int64
		if len(out) > 0 && (len(out) < len(turns) || start > 0) {
			older = out[0].first
		}
		return plainTurns(out), older, nil
	}
}

func plainTurns(recs []fleetTurnRec) []fleetChatTurn {
	out := make([]fleetChatTurn, len(recs))
	for i, t := range recs {
		out[i] = t.fleetChatTurn
	}
	return out
}

func (s *server) fleetChat(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	since := int64(0)
	if v := q.Get("since"); v != "" {
		p, err := strconv.ParseInt(v, 10, 64)
		if err != nil || p < 0 {
			http.Error(w, "bad since", http.StatusBadRequest)
			return
		}
		since = p
	}
	n := 50
	if v := q.Get("n"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 200 {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		n = p
	}
	w.Header().Set("Cache-Control", "no-store")
	empty := func(sid, reason string) {
		writeJSON(w, http.StatusOK, map[string]any{"turns": []fleetChatTurn{}, "pos": 0, "reset": true, "sessionId": sid, "reason": reason})
	}
	_, fl := s.discoverFleet()
	home, homeErr := s.fleetHome()
	sid, readOnly, path := fl.SessionID, false, ""
	if !fl.Alive {
		// FLEET is down: mirror the last session read-only, so the page
		// still shows what FLEET said last. Only for not_live: when two
		// sessions claim the name, nothing says which one to show.
		// The lookup is memoized (a miss for 30 s): this runs every poll.
		l, ok := readFleetLast(s.fleetLastPath())
		if fl.Code != "not_live" || !ok {
			empty("", fl.Reason)
			return
		}
		if path, _, ok = s.fleetTranscript(l.SessionID); !ok {
			empty("", fl.Reason)
			return
		}
		sid, readOnly = l.SessionID, true
	} else {
		if homeErr != nil {
			empty(sid, "no home dir")
			return
		}
		if path = findTranscript(home, sid); path == "" {
			empty(sid, "transcript not found")
			return
		}
	}
	turns, pos, reset, err := extractChatTurns(path, since, n, fleetChatTailBytes)
	if err != nil {
		empty(sid, "transcript unreadable")
		return
	}
	if turns == nil {
		turns = []fleetChatTurn{}
	}
	out := map[string]any{"turns": turns, "pos": pos, "reset": reset, "sessionId": sid}
	if readOnly {
		out["readOnly"] = true
		out["reason"] = fl.Reason
	}
	writeJSON(w, http.StatusOK, out)
}
