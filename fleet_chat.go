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

// extractChatTurns reads the transcript tail and returns the turns that
// changed at or after byte offset since — new turns, and turns whose text a
// later line of the same assistant message replaced — plus pos, the offset
// for the next call. A turn keeps the uuid of its first line, so the page can
// upsert. reset tells the page to replace its list instead of merging.
func extractChatTurns(path string, since int64, n int, tailBytes int64) ([]fleetChatTurn, int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, true, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, true, err
	}
	size := st.Size()
	if since > 0 && since == size {
		return nil, size, false, nil
	}
	start := int64(0)
	if size > tailBytes {
		start = size - tailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, true, err
	}
	rd := bufio.NewReaderSize(f, 64*1024)
	off := start
	if start > 0 { // drop the partial first line
		skipped, err := rd.ReadBytes('\n')
		off += int64(len(skipped))
		if err != nil {
			return nil, off, true, nil
		}
	}
	windowStart := off

	type turn struct {
		fleetChatTurn
		changedAt int64
	}
	var turns []turn
	idx := map[string]int{}
	pos := off
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil { // EOF: a line without '\n' is mid-write; leave it for the next poll
			break
		}
		lineOff := off
		off += int64(len(line))
		pos = off
		var e transcriptLine
		if json.Unmarshal(line, &e) != nil || e.IsMeta {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		text, ok := extractTurnText(e.Message.Content)
		if !ok {
			continue
		}
		if e.Type == "user" && strings.HasPrefix(strings.TrimSpace(text), "<") {
			continue
		}
		if e.Type == "assistant" {
			key := e.Message.ID + "\x00" + e.RequestID
			if i, ok := idx[key]; ok {
				turns[i].Text = text
				turns[i].changedAt = lineOff
				continue
			}
			idx[key] = len(turns)
		}
		turns = append(turns, turn{fleetChatTurn{UUID: e.UUID, Role: e.Type, At: e.Timestamp, Text: text}, lineOff})
	}

	reset := since <= 0 || since > size || since < windowStart
	var out []fleetChatTurn
	for _, t := range turns {
		if reset || t.changedAt >= since {
			out = append(out, t.fleetChatTurn)
		}
	}
	if len(out) > n {
		out, reset = out[len(out)-n:], true
	}
	return out, pos, reset, nil
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
	if !fl.Alive {
		empty("", fl.Reason)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		empty(fl.SessionID, "no home dir")
		return
	}
	path := findTranscript(home, fl.SessionID)
	if path == "" {
		empty(fl.SessionID, "transcript not found")
		return
	}
	turns, pos, reset, err := extractChatTurns(path, since, n, fleetChatTailBytes)
	if err != nil {
		empty(fl.SessionID, "transcript unreadable")
		return
	}
	if turns == nil {
		turns = []fleetChatTurn{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": turns, "pos": pos, "reset": reset, "sessionId": fl.SessionID})
}
