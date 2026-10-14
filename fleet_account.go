package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

// Account switch on the FLEET page: GET /fleet/accounts lists the saved
// claude and grok snapshots with their usage, POST /fleet/account/switch makes
// one the live account and tells FLEET. Nothing either route answers carries
// an email: names, active flags and windows only. Switch errors and warnings
// can name accounts by email, so their detail goes to stderr.

// What a running session does after a switch, tested 2026-10-14 against
// Claude Code 2.1.299 and grok 1.0.52 on this host: claude stats
// .credentials.json before every request and reloads it when the mtime
// changes, so it takes the new login on its next request; grok keeps its
// in-memory token until its own refresh (expires_at - 300 s, tokens live
// ~6 h) and then adopts the token on disk. Neither writes its old token back.
// The hint sits under "Switch <provider> to <name>?"; the note ends the event
// line FLEET reads.
const (
	fleetSwitchHintClaude = "Running Claude sessions switch to the new login on their next message. No restart needed."
	fleetSwitchHintGrok   = "Running Grok sessions keep the old login for up to 6 h. Restart them to switch now."
	fleetSwitchNoteClaude = "running sessions use the new login on their next message; resume the ones the old limit blocked"
	fleetSwitchNoteGrok   = "running grok sessions keep the old login up to 6 h; restart them to move now"
)

// fleetSwitchEventNote is the provider's closing note on the event line.
func fleetSwitchEventNote(provider string) string {
	if provider == accountToolGrok {
		return fleetSwitchNoteGrok
	}
	return fleetSwitchNoteClaude
}

func fleetSwitchHint(provider string) string {
	if provider == accountToolGrok {
		return fleetSwitchHintGrok
	}
	return fleetSwitchHintClaude
}

type fleetAccountRow struct {
	Name         string               `json:"name"`
	Active       bool                 `json:"active"`
	FiveHour     *usageWindowNumbers  `json:"fiveHour,omitempty"`
	SevenDay     *usageWindowNumbers  `json:"sevenDay,omitempty"`
	WeeklyScoped *usageWindowNumbers  `json:"weeklyScoped,omitempty"`
	Windows      []usageWindowNumbers `json:"windows,omitempty"`
	Stale        bool                 `json:"stale"`
}

type fleetAccountGroup struct {
	Accounts []fleetAccountRow `json:"accounts"`
	Hint     string            `json:"hint"`
	// ActiveUnsaved: an account is logged in, but no saved snapshot is it,
	// so no row is marked active.
	ActiveUnsaved bool `json:"activeUnsaved,omitempty"`
}

type fleetAccountsResponse struct {
	Claude fleetAccountGroup `json:"claude"`
	Grok   fleetAccountGroup `json:"grok"`
}

type fleetSwitchResult struct {
	OK       bool   `json:"ok"`
	Noop     bool   `json:"noop,omitempty"`
	Provider string `json:"provider,omitempty"`
	Name     string `json:"name,omitempty"`
	Previous string `json:"previous,omitempty"`
	// Event is idleDeliver's code for the line typed into FLEET: "sent" or a
	// refusal code.
	Event    string `json:"event,omitempty"`
	Warnings int    `json:"warnings,omitempty"`
	Code     string `json:"code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// fleetAccountNames is the provider's saved snapshot names, sorted. A name
// that accountNameOK refuses is dropped: such a file was not saved by this
// tool, and its name could be an email.
func (s *server) fleetAccountNames(provider string) ([]string, error) {
	var names []string
	var err error
	switch {
	case s.fleet.accountNames != nil:
		names, err = s.fleet.accountNames(provider)
	case provider == accountToolGrok:
		names, err = grokSnapshotNames()
	default:
		names, err = snapshotAccountNames()
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if accountNameOK(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, err
}

// fleetAccountCurrent is the live account's snapshot name, "" when none. A
// name accountNameOK refuses (it could be an email) is "", the same filter
// fleetAccountNames applies, so it never reaches a response or the log.
func (s *server) fleetAccountCurrent(provider string) string {
	var name string
	switch {
	case s.fleet.accountCurrent != nil:
		name = s.fleet.accountCurrent(provider)
	case provider == accountToolGrok:
		name = grokCurrentAccountName()
	default:
		name = currentAccountName()
	}
	if !accountNameOK(name) {
		return ""
	}
	return name
}

// fleetAccountLive is the live account's email, "" when none. Used only to
// decide activeUnsaved and to match usage; it never leaves the server.
func (s *server) fleetAccountLive(provider string) string {
	switch {
	case s.fleet.accountLive != nil:
		return s.fleet.accountLive(provider)
	case provider == accountToolGrok:
		return liveGrokEmail()
	default:
		return loadAccountEmail()
	}
}

func (s *server) fleetClaudeUsage() []claudeUsageNumbers {
	if s.fleet.claudeUsage != nil {
		return s.fleet.claudeUsage()
	}
	if s.numbers == nil {
		return nil
	}
	return s.numbers.build().Claude
}

func (s *server) fleetGrokLiveUsage() []providerUsageNumbers {
	if s.fleet.grokLiveUsage != nil {
		return s.fleet.grokLiveUsage()
	}
	return grokUsageNumbers(s.fleetNow(), false)
}

// fleetParkedGrok is the parked grok accounts' usage, refreshed at most once
// per usageRefreshInterval. grokFetchMu single-flights the fetch; a switch
// that lands during one bumps grokKnownGen, so that answer is not kept.
func (s *server) fleetParkedGrok() []KnownGrokAccountUsage {
	f := &s.fleet
	if f.grokKnown == nil {
		return nil
	}
	f.grokFetchMu.Lock()
	defer f.grokFetchMu.Unlock()
	now := s.fleetNow()
	f.grokKnownMu.Lock()
	if !f.grokKnownAt.IsZero() && now.Sub(f.grokKnownAt) < usageRefreshInterval {
		cached := f.grokKnownCache
		f.grokKnownMu.Unlock()
		return cached
	}
	gen := f.grokKnownGen
	f.grokKnownMu.Unlock()
	got := f.grokKnown()
	f.grokKnownMu.Lock()
	if f.grokKnownGen == gen {
		f.grokKnownCache, f.grokKnownAt = got, now
	}
	f.grokKnownMu.Unlock()
	return got
}

func (s *server) fleetInvalidateParkedGrok() {
	f := &s.fleet
	f.grokKnownMu.Lock()
	f.grokKnownCache, f.grokKnownAt = nil, time.Time{}
	f.grokKnownGen++
	f.grokKnownMu.Unlock()
}

// fleetFetchKnownGrok is the production grokKnown: one pass over the parked
// grok snapshots (network). It never rotates a parked refresh token: a page
// left open must not drive rotation from the always-on server, and the TUI's
// hub already rotates. A refused token just shows no usage for that account.
func fleetFetchKnownGrok() []KnownGrokAccountUsage {
	res, _ := fetchKnownGrokAccountsWith(false)
	return derefKnownGrokAccounts(res)
}

func copyWindow(w *usageWindowNumbers) *usageWindowNumbers {
	if w == nil {
		return nil
	}
	c := *w
	return &c
}

func (s *server) fleetClaudeAccounts() fleetAccountGroup {
	g := fleetAccountGroup{Accounts: []fleetAccountRow{}, Hint: fleetSwitchHint(accountToolClaude)}
	names, err := s.fleetAccountNames(accountToolClaude)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: claude account list failed: %v\n", err)
	}
	if len(names) == 0 {
		return g
	}
	current := s.fleetAccountCurrent(accountToolClaude)
	g.ActiveUnsaved = current == "" && s.fleetAccountLive(accountToolClaude) != ""
	usage := map[string]claudeUsageNumbers{}
	for _, e := range s.fleetClaudeUsage() {
		if e.Name != "" {
			usage[e.Name] = e
		}
	}
	for _, n := range names {
		row := fleetAccountRow{Name: n, Active: n == current}
		// Windows only: never account (an email) or credits.
		if e, ok := usage[n]; ok {
			row.FiveHour, row.SevenDay, row.WeeklyScoped = copyWindow(e.FiveHour), copyWindow(e.SevenDay), copyWindow(e.WeeklyScoped)
			row.Stale = e.Stale
		}
		g.Accounts = append(g.Accounts, row)
	}
	return g
}

func (s *server) fleetGrokAccounts() fleetAccountGroup {
	g := fleetAccountGroup{Accounts: []fleetAccountRow{}, Hint: fleetSwitchHint(accountToolGrok)}
	names, err := s.fleetAccountNames(accountToolGrok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: grok account list failed: %v\n", err)
	}
	if len(names) == 0 {
		return g
	}
	current := s.fleetAccountCurrent(accountToolGrok)
	live := s.fleetAccountLive(accountToolGrok)
	g.ActiveUnsaved = current == "" && live != ""
	now := s.fleetNow()
	parked := map[string]KnownGrokAccountUsage{}
	for _, a := range s.fleetParkedGrok() {
		parked[a.Name] = a
	}
	for _, n := range names {
		row := fleetAccountRow{Name: n, Active: n == current}
		if row.Active {
			// The hub's cache can still hold the account that was live
			// before a switch: use it only when it is this account.
			for _, e := range s.fleetGrokLiveUsage() {
				if emailMatchesLive(e.Account, live) && len(e.Windows) > 0 {
					row.Windows = append([]usageWindowNumbers(nil), e.Windows...)
					row.Stale = e.Stale
					break
				}
			}
		} else if a, ok := parked[n]; ok && a.Info != nil {
			if ws := providerWindows(a.Info.Windows); len(ws) > 0 {
				row.Windows = ws
				row.Stale = usageNumbersStale(now, a.FetchedAt, a.Stale)
			}
		}
		g.Accounts = append(g.Accounts, row)
	}
	return g
}

// fleetAccounts is GET /fleet/accounts.
func (s *server) fleetAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.fleetAuth(r) == fleetAuthNone {
		writeJSON(w, http.StatusUnauthorized, fleetSwitchResult{Code: "unauthorized", Error: "Unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, fleetAccountsResponse{Claude: s.fleetClaudeAccounts(), Grok: s.fleetGrokAccounts()})
}

// decodeFleetSwitchBody reads exactly one {"provider","name"} object: an
// unknown field, a top-level null, or trailing content is an error.
func decodeFleetSwitchBody(w http.ResponseWriter, r *http.Request) (provider, name string, err error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	var body *struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	}
	if err := dec.Decode(&body); err != nil || body == nil {
		return "", "", errors.New("bad json")
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", "", errors.New("bad json")
	}
	return body.Provider, body.Name, nil
}

// fleetAccountSwitch is POST /fleet/account/switch. Guards, in order: auth
// (cookie needs same origin), a strict body, a known provider, a name in that
// provider's fresh saved list, one switch at a time. Naming the active
// account is a no-op. A switch logs one web-switch line and types one event
// line into FLEET, after the switch lock is released.
func (s *server) fleetAccountSwitch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		writeJSON(w, http.StatusUnauthorized, fleetSwitchResult{Code: "unauthorized", Error: "Unauthorized"})
		return
	}
	if kind == fleetAuthCookie && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, fleetSwitchResult{Code: "origin", Error: "origin mismatch"})
		return
	}
	provider, name, err := decodeFleetSwitchBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, fleetSwitchResult{Code: "bad_request", Error: "bad request body"})
		return
	}
	if provider != accountToolClaude && provider != accountToolGrok {
		writeJSON(w, http.StatusBadRequest, fleetSwitchResult{Code: "bad_provider", Error: "provider must be claude or grok"})
		return
	}
	names, err := s.fleetAccountNames(provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: %s account list failed: %v\n", provider, err)
	}
	if !containsAccountName(names, name) {
		writeJSON(w, http.StatusBadRequest, fleetSwitchResult{Code: "unknown_account", Error: "no saved account by that name"})
		return
	}
	f := &s.fleet
	if !f.switchMu.TryLock() {
		writeJSON(w, http.StatusConflict, fleetSwitchResult{Code: "busy", Error: "another switch is running"})
		return
	}
	locked := true
	defer func() {
		if locked {
			f.switchMu.Unlock()
		}
	}()
	old := s.fleetAccountCurrent(provider)
	if old == name {
		writeJSON(w, http.StatusOK, fleetSwitchResult{OK: true, Noop: true, Provider: provider, Name: name, Previous: old})
		return
	}
	_, warnings, err := s.switchAccountTo(provider, name)
	if err != nil {
		// The error text can name accounts by email: stderr only.
		fmt.Fprintf(os.Stderr, "fleet: %s account switch to %s failed: %v\n", provider, name, err)
		if errors.Is(err, errUnknownAccount) {
			writeJSON(w, http.StatusBadRequest, fleetSwitchResult{Code: "unknown_account", Error: "no saved account by that name"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, fleetSwitchResult{Code: "switch_failed", Error: "switch failed; see the server log"})
		return
	}
	for _, wn := range warnings {
		fmt.Fprintf(os.Stderr, "fleet: %s account switch to %s: %s\n", provider, name, wn)
	}
	prev := old
	if prev == "" {
		prev = "-"
	}
	if err := s.appendFleetLog(fleetLogEntry{At: s.fleetNow().UTC().Format(time.RFC3339), Type: "web-switch",
		Text: provider + " account " + prev + " -> " + name}); err != nil {
		fmt.Fprintf(os.Stderr, "fleet: web-switch log append failed: %v\n", err)
	}
	if provider == accountToolGrok {
		s.fleetInvalidateParkedGrok()
	}
	f.switchMu.Unlock()
	locked = false

	event := s.fleetSwitchNotify(provider, old, name)
	writeJSON(w, http.StatusOK, fleetSwitchResult{OK: true, Provider: provider, Name: name, Previous: old,
		Event: event, Warnings: len(warnings)})
}

// fleetSwitchEventText is the line typed into FLEET after a switch. Names
// go through idleSanitize, so a name cannot forge the tag or break the line.
func fleetSwitchEventText(provider, old, name string) string {
	return fleetEventTag + " account " + provider + " switched from " + idleSanitize(old) +
		" to " + idleSanitize(name) + "; " + fleetSwitchEventNote(provider)
}

// fleetSwitchNotify types the switch event into FLEET and returns the
// delivery code: "sent" or idleDeliver's refusal code (target_missing when
// no FLEET is running and no target is configured).
func (s *server) fleetSwitchNotify(provider, old, name string) string {
	target := s.fleetEventTarget()
	text := fleetSwitchEventText(provider, old, name)
	var code string
	if s.fleet.deliverEvent != nil {
		code = s.fleet.deliverEvent(target, text)
	} else {
		code, _ = s.idleDeliver(target, text)
	}
	if code != "sent" {
		fmt.Fprintf(os.Stderr, "fleet: %s account switch event not delivered: code=%s target=%s\n", provider, code, fleetEventTargetName(target))
	}
	return code
}

// fleetEventTarget is where every server event line goes (idle events,
// account switches, ClickUp tickets): idle-events.json's target, enabled or
// not, read fresh — not the idle loop's copy, since that loop holds its lock
// across a whole delivery. "" means the FLEET session, which idleDeliver
// resolves on its own fresh session list at send time, so a FLEET restarted
// in a new tmux session is still reached. A missing or bad file is "".
func (s *server) fleetEventTarget() string {
	path, _, _ := s.idle.paths()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	c, err := parseIdleConfig(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: %s: %v; sending to FLEET instead\n", path, err)
		return ""
	}
	return c.Target
}

// fleetEventTargetName is a target for a log line: "" reads as FLEET.
func fleetEventTargetName(target string) string {
	if target == "" {
		return "FLEET"
	}
	return target
}
