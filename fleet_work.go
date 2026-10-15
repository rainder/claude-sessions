package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The FLEET page's working list. The server derives it from its own session
// list: every local tmux session that is not FLEET and not disabled. FLEET
// no longer writes working or waiting cards for it. A session that ends
// stays in a "recently ended" group for 24 h (fleetEndedTracker), and every
// ticket a row names carries its ClickUp status when the status cache has
// one (fleet_ticket_status.go). FLEET adds one short note per tmux session
// through state.json's top-level "notes" object.

const (
	fleetEndedKeep = 24 * time.Hour
	fleetNoteMax   = 200 // runes
	// fleetEndedGrace: a row must stay missing this long, over two looks or
	// more, before it counts as ended. Three observers read lists of
	// different age, and a torn grok registry read drops every grok row for
	// one poll.
	fleetEndedGrace = 15 * time.Second
)

// sessionTicketRe is ticketIDRe for any letter case: a tmux name or a
// worktree directory is often lower case (spawn-dr-5187-slug).
var sessionTicketRe = regexp.MustCompile(`(?i)\bDR-\d{1,6}\b`)

// sessionTickets lists the ticket ids a session names, in first-seen order
// over its tmux session name, worktree name, cwd and name; upper case, once
// each.
func sessionTickets(s Session) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, src := range []string{tmuxSessionPart(s.Tmux), s.WorktreeName, s.CWD, s.Name} {
		out = appendTickets(out, seen, src)
	}
	return out
}

// sessionOwnTickets is the tickets the session itself works on: only those
// in its tmux session name and worktree name. A cwd or a free-text name can
// mention other tickets ("follow-up to DR-100").
func sessionOwnTickets(s Session) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, src := range []string{tmuxSessionPart(s.Tmux), s.WorktreeName} {
		out = appendTickets(out, seen, src)
	}
	return out
}

func appendTickets(out []string, seen map[string]bool, src string) []string {
	for _, m := range sessionTicketRe.FindAllString(src, -1) {
		id := strings.ToUpper(m)
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// fleetWorkRow is one live session in /fleet/state's "sessions" list.
type fleetWorkRow struct {
	Tmux         string            `json:"tmux"` // tmux session name only
	PID          int               `json:"pid"`
	Name         string            `json:"name"`
	NameSource   string            `json:"nameSource"`
	Tickets      []string          `json:"tickets"`
	Tool         string            `json:"tool"` // claude or grok
	Model        string            `json:"model"`
	Status       string            `json:"status"`          // StatusDisplay; may be ""
	Since        string            `json:"since,omitempty"` // when not busy and known
	StartedAt    string            `json:"startedAt,omitempty"`
	TicketStatus map[string]string `json:"ticketStatus,omitempty"`
	Note         string            `json:"note,omitempty"`

	key   string    // pid:sessionId, the tracker's identity
	since time.Time // Since as a time; zero when unknown or busy
}

// fleetEndedRow is a derived row that went away, kept for fleetEndedKeep.
type fleetEndedRow struct {
	Tmux         string            `json:"tmux"`
	Name         string            `json:"name"`
	Tickets      []string          `json:"tickets"`
	Tool         string            `json:"tool"`
	Model        string            `json:"model"`
	EndedAt      string            `json:"endedAt"`
	TicketStatus map[string]string `json:"ticketStatus,omitempty"`
}

// fleetWorkStatusRank orders the list: waiting, idle, shell, no status, busy.
func fleetWorkStatusRank(st string) int {
	switch st {
	case "waiting":
		return 0
	case "idle":
		return 1
	case "shell":
		return 2
	case "busy":
		return 4
	}
	return 3
}

// fleetDisabled is the flags store's disabled bit (the disabled seam in
// tests).
func (s *server) fleetDisabled(sid string) bool {
	if sid == "" {
		return false
	}
	if d := s.fleet.disabled; d != nil {
		return d(sid)
	}
	return s.flags != nil && s.flags.Disabled(sid)
}

// fleetSelfTmux is every tmux session that holds FLEET: the one findFleet
// picks, and any session with a row named FLEET. The session view refuses
// them as fleet_self, so the working list leaves them out too.
func fleetSelfTmux(sessions []Session) map[string]bool {
	self := map[string]bool{}
	if fl, reason := findFleet(sessions); reason == "" && fl.Tmux != "" {
		self[tmuxSessionPart(fl.Tmux)] = true
	}
	for _, row := range sessions {
		if row.Tmux != "" && row.Name == fleetSessionName {
			self[tmuxSessionPart(row.Tmux)] = true
		}
	}
	return self
}

// fleetDerivedSessions filters sessions to the working list's rows: local,
// in tmux, not FLEET, not disabled. The idle ignore list does not apply:
// that list is about events, not about what runs. nil in, nil out (the list
// failed).
func (s *server) fleetDerivedSessions(sessions []Session) []Session {
	if sessions == nil {
		return nil
	}
	self := fleetSelfTmux(sessions)
	out := []Session{}
	for _, row := range sessions {
		part := tmuxSessionPart(row.Tmux)
		if row.Host != "" || row.Tmux == "" || !fleetSessionNameRe.MatchString(part) || self[part] || s.fleetDisabled(row.SessionID) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// fleetWorkRows builds the "sessions" rows, sorted, with ticket statuses from
// the cache. nil when the list failed.
func (s *server) fleetWorkRows(sessions []Session, now time.Time) []fleetWorkRow {
	derived := s.fleetDerivedSessions(sessions)
	if derived == nil {
		return nil
	}
	rows := make([]fleetWorkRow, 0, len(derived))
	for _, sess := range derived {
		r := fleetWorkRow{
			Tmux: tmuxSessionPart(sess.Tmux), PID: sess.PID, Name: sess.Name, NameSource: sess.NameSource,
			Tickets: sessionTickets(sess), Tool: "claude", Model: sess.Model, Status: sess.StatusDisplay(),
			key: idleKey(sess),
		}
		if sess.IsGrok() {
			r.Tool = "grok"
		}
		if r.Status != "busy" && sess.UpdatedAt > 0 {
			r.since = time.UnixMilli(sess.UpdatedAt)
			r.Since = r.since.UTC().Format(time.RFC3339)
		}
		if sess.StartedAt > 0 {
			r.StartedAt = time.UnixMilli(sess.StartedAt).UTC().Format(time.RFC3339)
		}
		r.TicketStatus = s.fleet.tickets.statuses(r.Tickets, nil, now)
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := fleetWorkStatusRank(rows[i].Status), fleetWorkStatusRank(rows[j].Status)
		if a != b {
			return a < b
		}
		if rows[i].Tmux != rows[j].Tmux {
			return rows[i].Tmux < rows[j].Tmux
		}
		return rows[i].PID < rows[j].PID
	})
	return rows
}

// fleetSessionKeys is every local row's pid:sessionId. The tracker uses it
// to tell a row that left the working list while still running (disabled,
// for example) from one that ended.
func fleetSessionKeys(sessions []Session) map[string]bool {
	out := make(map[string]bool, len(sessions))
	for _, row := range sessions {
		if row.Host == "" {
			out[idleKey(row)] = true
		}
	}
	return out
}

// fleetNotes reads state.json's "notes": tmux session name → FLEET's short
// note. A key that is not a session name, a value that is not a string or
// holds a control character, is dropped; a long note is cut to fleetNoteMax
// runes.
func fleetNotes(state map[string]json.RawMessage) map[string]string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(state["notes"], &raw) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		if !fleetSessionNameRe.MatchString(k) {
			continue
		}
		var note string
		if json.Unmarshal(v, &note) != nil || !utf8.ValidString(note) {
			continue
		}
		if strings.IndexFunc(note, unicode.IsControl) >= 0 {
			continue
		}
		note = strings.TrimSpace(note)
		if utf8.RuneCountInString(note) > fleetNoteMax {
			note = strings.TrimSpace(string([]rune(note)[:fleetNoteMax]))
		}
		if note != "" {
			out[k] = note
		}
	}
	return out
}

// fleetApplyNotes puts each note on the live row in its tmux session. A
// note for a session that is not a live row is not shown.
func fleetApplyNotes(rows []fleetWorkRow, notes map[string]string) {
	for i := range rows {
		rows[i].Note = notes[rows[i].Tmux]
	}
}

// ---- recently ended -------------------------------------------------------

// fleetEndedLive is a row the tracker saw live, and when. missingSince is
// the first look that did not see it since then; zero while it is seen.
type fleetEndedLive struct {
	row          fleetWorkRow
	lastSeen     time.Time
	missingSince time.Time
}

// fleetEndedTracker remembers the working rows it saw, and turns a row that
// goes away into an ended row. Only the ended list is saved (path, 0600,
// atomic), so a restart keeps the group; the live map starts empty, and a
// session that ends while the server is down is not seen.
type fleetEndedTracker struct {
	path string // "" → memory only; cmdServer sets fleetEndedPathDefault

	mu     sync.Mutex
	loaded bool
	live   map[string]fleetEndedLive
	ended  []fleetEndedRow
}

type fleetEndedFile struct {
	Ended []fleetEndedRow `json:"ended"`
}

func fleetEndedPathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "fleet-ended.json")
	}
	return ""
}

// fleetTicketOK is the shape of a ticket id passed to cu or typed into an
// event line.
var fleetTicketOK = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-\d{1,7}$`)

// validEndedRow keeps a loaded row only when every field is one the tracker
// itself could have written.
func validEndedRow(r *fleetEndedRow) bool {
	if !fleetSessionNameRe.MatchString(r.Tmux) {
		return false
	}
	if _, err := time.Parse(time.RFC3339, r.EndedAt); err != nil {
		return false
	}
	tickets := []string{}
	for _, t := range r.Tickets {
		if fleetTicketOK.MatchString(t) {
			tickets = append(tickets, t)
		}
	}
	r.Tickets = tickets
	for t, st := range r.TicketStatus {
		if !fleetTicketOK.MatchString(t) || !ticketStatusRe.MatchString(st) {
			delete(r.TicketStatus, t)
		}
	}
	return true
}

func (t *fleetEndedTracker) loadLocked() {
	if t.loaded {
		return
	}
	t.loaded = true
	t.live = map[string]fleetEndedLive{}
	if t.path == "" {
		return
	}
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var f fleetEndedFile
	if json.Unmarshal(data, &f) != nil {
		return
	}
	for _, r := range f.Ended {
		if validEndedRow(&r) {
			t.ended = append(t.ended, r)
		}
	}
}

func (t *fleetEndedTracker) saveLocked() {
	if t.path == "" {
		return
	}
	f := fleetEndedFile{Ended: t.ended}
	if f.Ended == nil {
		f.Ended = []fleetEndedRow{}
	}
	data, err := json.Marshal(f)
	if err == nil {
		err = writeFileAtomic(t.path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: cannot save the ended list: %v\n", err)
	}
}

// observe takes one successful look at the working rows. A row seen before
// and missing now is kept with its first miss time; once it is still missing
// at least fleetEndedGrace after that, it becomes an ended row stamped with
// when it was last seen. Seeing it again clears the miss. A missing row is
// dropped without ending when a live row runs in the same tmux session, or
// when the session itself still runs (all holds every local row's key: a
// session that was disabled left the list, it did not end). Ended rows older than fleetEndedKeep, and
// any whose tmux session is live again, are dropped; one per tmux session,
// the newest. rows nil (the list failed) is a no-op. The file is written
// only when the ended list changed.
func (t *fleetEndedTracker) observe(rows []fleetWorkRow, all map[string]bool, now time.Time) {
	if rows == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked()
	liveTmux := map[string]bool{}
	cur := make(map[string]fleetEndedLive, len(rows))
	for _, r := range rows {
		liveTmux[r.Tmux] = true
		cur[r.key] = fleetEndedLive{row: r, lastSeen: now}
	}
	changed := false
	for k, prev := range t.live {
		if _, ok := cur[k]; ok || all[k] || liveTmux[prev.row.Tmux] {
			continue
		}
		if prev.missingSince.IsZero() {
			prev.missingSince = now
		}
		if now.Sub(prev.missingSince) < fleetEndedGrace {
			cur[k] = prev // one miss is not an end
			continue
		}
		if t.addLocked(fleetEndedRow{
			Tmux: prev.row.Tmux, Name: prev.row.Name, Tickets: prev.row.Tickets, Tool: prev.row.Tool,
			Model: prev.row.Model, EndedAt: prev.lastSeen.UTC().Format(time.RFC3339), TicketStatus: prev.row.TicketStatus,
		}) {
			changed = true
		}
	}
	t.live = cur
	kept := t.ended[:0]
	for _, e := range t.ended {
		at, _ := time.Parse(time.RFC3339, e.EndedAt)
		if now.Sub(at) > fleetEndedKeep || liveTmux[e.Tmux] {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	t.ended = kept
	if changed {
		t.saveLocked()
	}
}

// addLocked adds e, keeping only the newest ended row per tmux session. It
// reports whether the list changed.
func (t *fleetEndedTracker) addLocked(e fleetEndedRow) bool {
	for i, old := range t.ended {
		if old.Tmux != e.Tmux {
			continue
		}
		if old.EndedAt > e.EndedAt { // RFC3339 UTC sorts as text
			return false
		}
		t.ended = append(t.ended[:i:i], t.ended[i+1:]...)
		break
	}
	if e.Tickets == nil {
		e.Tickets = []string{}
	}
	t.ended = append(t.ended, e)
	return true
}

// list is the ended rows inside fleetEndedKeep, newest first ([] when
// none). The caller may change the copies.
func (t *fleetEndedTracker) list(now time.Time) []fleetEndedRow {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked()
	out := []fleetEndedRow{}
	for _, e := range t.ended {
		at, err := time.Parse(time.RFC3339, e.EndedAt)
		if err != nil || now.Sub(at) > fleetEndedKeep {
			continue
		}
		c := e
		c.Tickets = append([]string{}, e.Tickets...)
		if e.TicketStatus != nil {
			c.TicketStatus = make(map[string]string, len(e.TicketStatus))
			for k, v := range e.TicketStatus {
				c.TicketStatus[k] = v
			}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].EndedAt != out[j].EndedAt {
			return out[i].EndedAt > out[j].EndedAt
		}
		return out[i].Tmux < out[j].Tmux
	})
	return out
}

// fleetObserveWork derives the working rows from a successful session list
// and lets the tracker see them. nil sessions (a failed list) does nothing.
func (s *server) fleetObserveWork(sessions []Session, now time.Time) []fleetWorkRow {
	rows := s.fleetWorkRows(sessions, now)
	if rows != nil {
		s.fleet.ended.observe(rows, fleetSessionKeys(sessions), now)
	}
	return rows
}

// fleetEndedRows is the ended list with each ticket status taken from the
// live cache when it has one, else the status stored when the row ended.
func (s *server) fleetEndedRows(now time.Time) []fleetEndedRow {
	rows := s.fleet.ended.list(now)
	for i := range rows {
		rows[i].TicketStatus = s.fleet.tickets.statuses(rows[i].Tickets, rows[i].TicketStatus, now)
	}
	return rows
}
