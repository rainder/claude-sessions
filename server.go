package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTP server mode (-s flag). Exposes this host's sessions over JSON+bearer-
// auth so a client running elsewhere can include them in its live view.

// defaultServerPort is the port the server binds and clip-request POSTs to.
const defaultServerPort = 8765

// activeServerPort is the port this process's server is (or would be) reachable
// on. cmdServer sets it from its resolved --port so SpawnNew — called from the
// server without the port in hand — can embed the right port in the tmux paste
// binding. Stays at the default in non-server contexts (local CLI/TUI).
var activeServerPort = defaultServerPort

// actionResult is the JSON shape returned by mutating endpoints.
// Mirrors the bash version so existing scripts/clients keep working.
type actionResult struct {
	OK    bool   `json:"ok"`
	Tmux  string `json:"tmux,omitempty"`  // tmux session name for migrate/new
	Error string `json:"error,omitempty"` // human-readable failure reason
	// Code is a machine-readable failure kind for the cases a client has to act
	// on differently rather than just display. Omitted on success and on
	// failures that are only worth showing, so the desktop — which decodes this
	// same struct and ignores what it doesn't know — sees nothing new.
	Code string `json:"code,omitempty"`
	// Worktree is set by kill when the killed session was the last one running
	// in a git worktree, so the client can offer to remove it. Omitted
	// otherwise; older clients ignore it, and a new client against an old
	// server simply never sees it.
	Worktree *worktreeInfo `json:"worktree,omitempty"`
}

// worktreeInfo describes a worktree a kill has just left idle.
type worktreeInfo struct {
	Path string `json:"path"` // worktree checkout root
	Name string `json:"name"` // last path element, for the prompt
}

// The two values actionResult.Code ever carries. Both mean "your row is stale,
// refresh" and neither means "retry" — a client that retries blind on either is
// firing at whatever now occupies the PID.
const (
	// codeSessionMismatch: the PID is live, but it holds a different session
	// than the one the client named. A recycled tmux pane hands the same PID to
	// somebody else, so a matching PID on its own proves nothing.
	codeSessionMismatch = "session_mismatch"
	// codeNotLive: no live Claude session at that PID at all.
	codeNotLive = "not_live"
)

// sessionIDPrecondition reads the optional {"session_id": "..."} body that kill
// and migrate accept. "" means no precondition, which is the pre-existing
// behaviour: resolve the PID against this host's own list and act on whatever
// is there.
//
// Two rules, and the tension between them is the whole design.
//
// An empty request body is "no precondition", not a malformed one. Every
// scripted caller and every pre-existing test posts with no body at all, and
// the desktop posts a literal `{}`; json.Decode reports both as io.EOF. That
// one error is tolerated explicitly.
//
// Everything else is strict, because every loose form fails *open* — it decodes
// to an empty id, which this function's own contract then reads as "no
// precondition", and a guarded kill silently becomes a blind one. So an unknown
// field is rejected rather than ignored (`sessionId` is exactly the spelling the
// iOS model uses, and one camelCase typo in a client would otherwise disarm the
// guard), an explicit null is rejected rather than read as absent, and trailing
// content after the object is rejected rather than skipped. A caller that means
// "no precondition" has two ways to say so that cost it nothing.
//
// Not covered: duplicate keys. Go's decoder applies last-one-wins without
// surfacing that it saw two, and detecting it would mean parsing the body twice
// for a case no client produces.
func sessionIDPrecondition(w http.ResponseWriter, r *http.Request) (string, error) {
	// Decoded through a *pointer to* the struct, because a top-level `null` is a
	// silent no-op when decoded into a struct value: it leaves every field zero
	// and returns no error, so `null` came back as "no precondition" and
	// disarmed the guard exactly like an unknown field did. Into a pointer it
	// leaves the pointer nil, which is distinguishable from `{}`.
	//
	// RawMessage on the field for the same reason one level down: only a raw
	// value can tell an absent key from an explicit null.
	var body *struct {
		SessionID json.RawMessage `json:"session_id"`
	}
	// Bounded: this is parsed before anything else reads the request, and the
	// only thing in it is a uuid.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		return "", err
	}
	// Exactly one JSON value, nothing after it. Token() rather than More():
	// More() only reports a well-formed next element, so it misses trailing
	// garbage that is not itself valid JSON, while a clean stream is the one
	// case where Token() reports io.EOF.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("unexpected trailing json")
	}
	if body == nil {
		return "", fmt.Errorf("body must be an object")
	}
	if len(body.SessionID) == 0 {
		return "", nil // key absent — `{}` — is "no precondition"
	}
	if string(body.SessionID) == "null" {
		// A client that meant to send an id and computed nothing. Saying so
		// beats silently disarming the guard.
		return "", fmt.Errorf("session_id must be a string")
	}
	var id string
	if err := json.Unmarshal(body.SessionID, &id); err != nil {
		return "", err
	}
	return id, nil
}

type sessionFlight struct {
	done       chan struct{}
	err        error
	generation uint64
}

type sessionCache struct {
	mu               sync.Mutex
	sessions         []Session
	completedAt      time.Time
	valid            bool
	cachedGeneration uint64
	generation       uint64
	flight           *sessionFlight
	now              func() time.Time
}

// spawnDedupeMax / spawnDedupeTTL bound the request_id cache: at most this many
// remembered spawns, forgotten this long after they finished. The cap mirrors
// the device registry's; the TTL is generous next to the window it exists for —
// a phone that gave up at 30s and a user who tapped again. In memory only, so a
// restart forgets: the cost of that is one duplicate spawn inside a retry window
// measured in seconds.
const (
	spawnDedupeMax = 32
	spawnDedupeTTL = 10 * time.Minute
)

// spawnFlight is one spawn keyed by request_id: in flight until done closes,
// then holding the result a replay serves.
type spawnFlight struct {
	done   chan struct{}
	result actionResult
	// finishedAt is zero while the spawn is still running. That is also what
	// keeps expiry and eviction off it: dropping an entry a joiner is waiting on
	// would turn the join into a second spawn of the same user action, which is
	// the one thing request_id exists to prevent.
	finishedAt time.Time
}

// spawnDedupe makes POST /sessions/new idempotent per request_id. Every other
// mutating endpoint is naturally safe to repeat — a second kill finds nothing
// live, a second resume is refused with 409, a second worktree remove fails
// validation — and spawn is the one that would happily create a second tmux
// session and a second Claude process in the same directory.
//
// Single-flight with a bounded memory of results, the same shape sessionCache
// uses for /sessions.
type spawnDedupe struct {
	mu      sync.Mutex
	entries map[string]*spawnFlight
	now     func() time.Time
}

func (d *spawnDedupe) timeNow() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *spawnDedupe) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// spawnClaim is what begin decided about a request_id.
type spawnClaim int

const (
	// spawnClaimed: this caller owns the spawn and must call finish.
	spawnClaimed spawnClaim = iota
	// spawnJoined: someone else owns it; wait on flight.done and serve
	// flight.result.
	spawnJoined
	// spawnClaimRefused: the ledger is full of spawns that are all still
	// running. Retryable, and the caller still holds its id.
	spawnClaimRefused
)

// begin claims id for this caller, hands back the flight already holding it, or
// refuses because there is no room left that can be freed safely.
func (d *spawnDedupe) begin(id string) (*spawnFlight, spawnClaim) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing := d.entries[id]; existing != nil {
		// Expiry is checked here as well as in prune, because prune runs on
		// insert and this lookup happens first.
		if existing.finishedAt.IsZero() || d.timeNow().Before(existing.finishedAt.Add(spawnDedupeTTL)) {
			return existing, spawnJoined
		}
		delete(d.entries, id)
	}
	d.prune()
	// prune evicts finished entries only. If the map is still at the cap, every
	// remaining entry is a spawn that is still running, and evicting one of those
	// would strand its joiners into starting a second spawn of the same user
	// action — precisely what request_id exists to prevent. Refuse the new id
	// instead: the bound stays a bound, and the caller can retry with the same id
	// once a slot frees.
	if len(d.entries) >= spawnDedupeMax {
		return nil, spawnClaimRefused
	}
	if d.entries == nil {
		d.entries = make(map[string]*spawnFlight)
	}
	flight := &spawnFlight{done: make(chan struct{})}
	d.entries[id] = flight
	return flight, spawnClaimed
}

// prune drops expired entries and then evicts oldest-first until there is room
// for one more. In-flight entries are never candidates for either. Called under
// d.mu from begin.
func (d *spawnDedupe) prune() {
	now := d.timeNow()
	for id, flight := range d.entries {
		if !flight.finishedAt.IsZero() && !now.Before(flight.finishedAt.Add(spawnDedupeTTL)) {
			delete(d.entries, id)
		}
	}
	for len(d.entries) >= spawnDedupeMax {
		oldestID := ""
		var oldest time.Time
		for id, flight := range d.entries {
			if flight.finishedAt.IsZero() {
				continue
			}
			if oldestID == "" || flight.finishedAt.Before(oldest) {
				oldestID, oldest = id, flight.finishedAt
			}
		}
		if oldestID == "" {
			// Everything is still running. Concurrency is holding the map above
			// the cap, not accumulation, and it drains as those spawns finish.
			return
		}
		delete(d.entries, oldestID)
	}
}

// finish publishes result to every joiner. A failure is forgotten immediately
// instead of remembered: nothing was created, so a retry should genuinely
// re-run — caching a transient tmux failure for ten minutes would leave the user
// unable to retry a spawn that would now work. Joiners already holding the
// flight still get the failure, so a concurrent pair still spawns once.
func (d *spawnDedupe) finish(id string, flight *spawnFlight, result actionResult) {
	d.mu.Lock()
	flight.result = result
	flight.finishedAt = d.timeNow()
	if !result.OK && d.entries[id] == flight {
		delete(d.entries, id)
	}
	d.mu.Unlock()
	close(flight.done)
}

// validSpawnRequestID reports whether id is an acceptable idempotency key:
// 8 to 128 characters of [A-Za-z0-9_-]. It becomes a map key held in memory for
// ten minutes and reaches nothing else; bounding its length and character set is
// what keeps it that way.
func validSpawnRequestID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

type server struct {
	token string
	host  string
	// hostSnapshot returns this host's latest resource usage; nil yields an
	// empty HostUsage so old clients and tests without a hub still get 200.
	hostSnapshot func() HostUsage
	// usageSnapshot returns this host's account rate-limit usage paired with the
	// account it belongs to. nil (no hub) or a nil return (no fetch yet) omits
	// the "usage" key, so old clients and tests without a hub still get 200.
	usageSnapshot func() *AccountUsage
	// codexUsageSnapshot returns this host's OpenAI Codex account usage. nil (no
	// hub) or a nil return (no fetch yet, or no Codex auth) omits the
	// "codex_usage" key — the account email rides in the snapshot itself.
	codexUsageSnapshot func() *CodexAccountUsage
	// previewLoader is the preview backend; nil means LoadPreview. Tests inject
	// a stub to assert bounds and header wiring without touching tmux.
	previewLoader func(int, PreviewLimits) (PreviewResult, error)

	// collect/terminate are injectable seams for tests; nil in production,
	// where they fall back to CollectLocal / KillSession.
	collect   func() ([]Session, error)
	terminate func(Session) error
	// removeTree removes a worktree checkout; nil falls back to RemoveWorktree,
	// so tests exercise the handler without a real git repo.
	removeTree func(string) error
	// spawn creates a new tmux session; nil falls back to SpawnNew. Same seam
	// pattern as the three above, and the only way the request_id dedupe's
	// concurrency can be tested without really starting tmux.
	spawn func(cwd, name, command, suffix string) (string, error)
	// attest re-reads one PID's own session file; nil falls back to
	// readSessionByPID. Used for the last-moment identity check before a
	// destructive act, and separate from collect because it must be the cheapest
	// possible read — no tmux mapping, no transcript scan, nothing that would
	// reintroduce the staleness it exists to close.
	attest func(int) (Session, bool)

	// devices is the push registry; nil when this server was built without
	// notification support, in which case the /devices routes report 503 rather
	// than panicking.
	devices *DeviceStore

	// pairing is the in-flight pairing offer, nil unless `pair` has armed one.
	// Guarded because arm and exchange arrive on different connections.
	pairingMu sync.Mutex
	pairing   *pairingCode

	sessionCache sessionCache

	// spawns dedupes POST /sessions/new by request_id.
	spawns spawnDedupe

	// paste is the remote-image-paste broker (see paste.go); pb() lazily
	// initializes it so both cmdServer and tests get a working broker.
	pasteOnce sync.Once
	paste     *pasteBroker
}

func (s *server) authed(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) == 1
}

func (s *server) collectLocalRaw() ([]Session, error) {
	if s.collect != nil {
		return s.collect()
	}
	return CollectLocal()
}

// collectLocal returns this host's live sessions. Disabled state is entirely
// client-side now (see state.go / the TUI overlay), so the server no longer
// annotates it — every session it reports has Disabled=false.
func (s *server) collectLocal() ([]Session, error) {
	return s.collectLocalRaw()
}

func (c *sessionCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (s *server) cachedSessions() ([]Session, error) {
	cache := &s.sessionCache
	for {
		cache.mu.Lock()
		if cache.valid && cache.cachedGeneration == cache.generation && cache.timeNow().Before(cache.completedAt.Add(time.Second)) {
			sessions := cache.sessions
			cache.mu.Unlock()
			return sessions, nil
		}
		if flight := cache.flight; flight != nil {
			cache.mu.Unlock()
			<-flight.done

			cache.mu.Lock()
			err := flight.err
			currentGeneration := cache.generation
			flightGeneration := flight.generation
			cache.mu.Unlock()
			if err != nil && currentGeneration == flightGeneration {
				return nil, err
			}
			continue
		}

		flight := &sessionFlight{done: make(chan struct{})}
		cache.flight = flight
		cache.mu.Unlock()

		for {
			cache.mu.Lock()
			generation := cache.generation
			cache.mu.Unlock()

			sessions, err := s.collectLocal()
			completedAt := cache.timeNow()

			cache.mu.Lock()
			if cache.generation != generation {
				cache.mu.Unlock()
				continue
			}
			flight.err = err
			flight.generation = generation
			if err == nil {
				cache.sessions = sessions
				cache.completedAt = completedAt
				cache.cachedGeneration = generation
				cache.valid = true
			}
			cache.flight = nil
			close(flight.done)
			cache.mu.Unlock()
			return sessions, err
		}
	}
}

func (s *server) invalidateSessions() {
	cache := &s.sessionCache
	cache.mu.Lock()
	cache.generation++
	cache.sessions = nil
	cache.completedAt = time.Time{}
	cache.valid = false
	cache.mu.Unlock()
}

func (s *server) terminateSession(target Session) error {
	if s.terminate != nil {
		return s.terminate(target)
	}
	return KillSession(target)
}

func (s *server) removeWorktreeAt(path string) error {
	if s.removeTree != nil {
		return s.removeTree(path)
	}
	return RemoveWorktree(path)
}

func (s *server) spawnNew(cwd, name, command, suffix string) (string, error) {
	if s.spawn != nil {
		return s.spawn(cwd, name, command, suffix)
	}
	return SpawnNewWithSuffix(cwd, name, command, suffix)
}

// spawnSuffix derives the tmux name suffix for a request_id, so the same id
// always names the same session.
//
// That matters when a partial spawn's cleanup fails: the ledger deliberately
// forgets failures so a retry re-runs, and with a random suffix the retry would
// build a second session beside the orphan. Derived, it collides with the
// survivor and tmux refuses — an error the user sees instead of a duplicate they
// do not. "" (no request_id, so no idempotency asked for) keeps the random slug.
func spawnSuffix(requestID string) string {
	if requestID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(requestID))
	return hex.EncodeToString(sum[:])[:6]
}

func (s *server) attestSession(pid int) (Session, bool) {
	if s.attest != nil {
		return s.attest(pid)
	}
	return readSessionByPID(pid)
}

// reattest re-checks the PID/session-id binding immediately before a
// destructive act, and returns a refusal if anything has moved.
//
// resolveLivePID compares against s.collectLocal(), which walks every session
// file, maps tmux panes, resolves git roots and scans transcripts for cost and
// agent counts. That is real I/O, and the answer it returns describes the world
// as it was when the walk started. Between there and the syscall a pane can be
// recycled and hand the PID to someone else — so the guard is re-run against
// the one file that actually establishes identity, as late as possible. The
// residual window is then the syscall itself rather than the whole pipeline.
//
// Only when the client named a session: with no precondition there is nothing
// to attest against, and the unconditional path must not acquire a new way to
// fail.
func (s *server) reattest(pid int, wantSession string) *actionResult {
	if wantSession == "" {
		return nil
	}
	sess, ok := s.attestSession(pid)
	if !ok {
		return &actionResult{
			Error: fmt.Sprintf("PID %d is not a live Claude session", pid),
			Code:  codeNotLive,
		}
	}
	if sess.SessionID != wantSession {
		return &actionResult{
			Error: fmt.Sprintf("PID %d is a different session now", pid),
			Code:  codeSessionMismatch,
		}
	}
	return nil
}

// resolveLivePID finds this host's own row for pid and checks it against an
// optional client-supplied session id. On success it returns the row plus the
// whole collected list (kill needs it to decide the worktree); on refusal it
// returns the envelope to write and nothing has been touched.
//
// The comparison target is always the server's freshly collected list, never
// anything the client asserted about the target — the same rule the kill handler
// has always followed, extended rather than weakened.
func (s *server) resolveLivePID(pid int, wantSession string) (*Session, []Session, *actionResult) {
	sessions, err := s.collectLocal()
	if err != nil {
		return nil, nil, &actionResult{Error: err.Error()}
	}
	for i := range sessions {
		if sessions[i].PID != pid {
			continue
		}
		target := &sessions[i]
		// A phone acts on a list it may have polled minutes ago, and a recycled
		// pane hands that PID to a different session. Refuse rather than act on
		// the wrong one. The refusal deliberately does not name what is there
		// now: the client learns its target is gone, not who replaced it.
		if wantSession != "" && target.SessionID != wantSession {
			return nil, nil, &actionResult{
				Error: fmt.Sprintf("PID %d is a different session now", pid),
				Code:  codeSessionMismatch,
			}
		}
		return target, sessions, nil
	}
	return nil, nil, &actionResult{
		Error: fmt.Sprintf("PID %d is not a live Claude session", pid),
		Code:  codeNotLive,
	}
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *server) sessions(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sessions, err := s.cachedSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hostUsage := HostUsage{}
	if s.hostSnapshot != nil {
		hostUsage = s.hostSnapshot()
	}
	resp := map[string]any{
		"hostname":  s.host,
		"ts":        time.Now().Unix(),
		"hostUsage": hostUsage,
		"sessions":  sessions,
	}
	// "usage" is optional: present only once this host's poller has a snapshot,
	// so a client can attribute the limits to this host's account. Omitted when
	// absent — older clients ignore it and it never nulls out the response.
	if s.usageSnapshot != nil {
		if u := s.usageSnapshot(); u != nil {
			resp["usage"] = u
		}
	}
	// "codex_usage" is the OpenAI Codex equivalent, same optionality.
	if s.codexUsageSnapshot != nil {
		if u := s.codexUsageSnapshot(); u != nil {
			resp["codex_usage"] = u
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) cwdSuggestions(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, struct {
		Home        string          `json:"home"`
		Suggestions []cwdSuggestion `json:"suggestions"`
	}{Home: home, Suggestions: collectCwdSuggestions()})
}

// presets lists this host's configured command preset names (not the command
// text itself — that's local shell input a remote client has no business
// seeing or replaying). Used by remote clients to validate `--command` and
// populate the new-session picker without guessing at this host's config.
func (s *server) presets(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	presets, err := LoadCommandPresets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Presets []CommandPreset `json:"presets"`
	}{Presets: presets})
}

// resumable returns this host's resumable (past, ended) sessions, collected
// in-process — the same primitive the local TUI path uses.
func (s *server) resumable(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Sessions []ResumableSession `json:"sessions"`
	}{Sessions: CollectResumable()})
}

// resume spawns `claude --resume <id>` in a fresh tmux session for the given
// session id + cwd. A session that's already live is refused with 409;
// validation and the spawn go through the shared ResumeSession primitive.
func (s *server) resume(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	tname, err := ResumeSession(body.SessionID, body.CWD)
	if err != nil {
		if errors.Is(err, errResumeSessionLive) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, actionResult{OK: true, Tmux: tname})
}

func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	limits, err := previewLimitsFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	load := s.previewLoader
	if load == nil {
		load = LoadPreview
	}
	result, err := load(pid, limits)
	if err != nil {
		if errors.Is(err, errSessionEnded) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Claude-Sessions-Preview-Source", result.Source)
	w.Header().Set("X-Claude-Sessions-Preview-Label", result.Label)
	_, _ = w.Write([]byte(result.Content))
}

// previewLimitsFromRequest reads optional lines/bytes query params, defaulting
// to DefaultPreviewLimits. Values are accepted only within 1..2000 lines and
// 1024..524288 bytes; anything else (non-numeric, negative, out of range) is an
// error the handler turns into 400.
func previewLimitsFromRequest(r *http.Request) (PreviewLimits, error) {
	limits := DefaultPreviewLimits()
	q := r.URL.Query()
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 2000 {
			return PreviewLimits{}, fmt.Errorf("bad lines value: %s", v)
		}
		limits.MaxLines = n
	}
	if v := q.Get("bytes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1024 || n > 524288 {
			return PreviewLimits{}, fmt.Errorf("bad bytes value: %s", v)
		}
		limits.MaxBytes = n
	}
	return limits, nil
}

func (s *server) tmuxInfo(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"tmux": tmuxSessionForPID(pid),
	})
}

func (s *server) kill(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	// session_id is an optional precondition, checked below against the row the
	// server resolves for itself. Absent means today's behaviour: kill whatever
	// is at this PID.
	wantSession, err := sessionIDPrecondition(w, r)
	if err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// Trust only server-collected metadata: resolve the PID against this host's
	// own sessions and terminate that full row. The request body carries no
	// tmux metadata — the client cannot steer which target we signal, and the
	// session id it may supply can only ever narrow the target, never widen it.
	target, sessions, refusal := s.resolveLivePID(pid, wantSession)
	if refusal != nil {
		writeJSON(w, http.StatusOK, *refusal)
		return
	}
	// Whether this kill empties a worktree is decided here, from the same
	// server-collected list: the client never gets to assert it. It is decided
	// after the precondition, so a refused kill never invites the client to
	// remove a worktree that is still in use.
	worktree := worktreeCleanupTarget(*target, sessions)
	// Last thing before the signal: confirm the PID still holds the session the
	// client named. Everything above ran against a snapshot that cost real I/O
	// to build.
	if refusal := s.reattest(pid, wantSession); refusal != nil {
		writeJSON(w, http.StatusOK, *refusal)
		return
	}
	// The row handed to terminateSession stays the enriched one: its tmux
	// metadata is what lets KillSession kill the whole pane group by name rather
	// than signalling a bare PID, and the attestation above is what confirms that
	// row still describes what lives at this PID.
	if err := s.terminateSession(*target); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	s.invalidateSessions()
	result := actionResult{OK: true}
	if worktree != "" {
		result.Worktree = &worktreeInfo{Path: worktree, Name: filepath.Base(worktree)}
	}
	writeJSON(w, http.StatusOK, result)
}

// removeWorktree handles POST /worktree/remove. The path arrives from a client,
// so it is validated (absolute, clean, worktree-shaped, a real git worktree)
// before anything touches disk, and re-checked against the live session list so
// a session started since the kill blocks the removal.
func (s *server) removeWorktree(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if err := validateWorktreePath(req.Path); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	sessions, err := s.collectLocal()
	if err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	for _, sess := range sessions {
		if worktreeRoot(sess.CWD) == req.Path {
			writeJSON(w, http.StatusOK, actionResult{
				Error: fmt.Sprintf("worktree still in use by PID %d", sess.PID),
			})
			return
		}
	}
	if err := s.removeWorktreeAt(req.Path); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResult{OK: true})
}

func (s *server) migrate(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	wantSession, err := sessionIDPrecondition(w, r)
	if err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// Only collect when a session id was supplied, so the desktop's migrate
	// costs exactly what it always did and this change is provably a no-op for
	// it. When the client does name a session, resolve through collectLocal
	// rather than readSessionByPID: it is the same trusted source kill uses, and
	// it goes through the s.collect seam, which is the only reason this is
	// testable without real session files on disk.
	//
	// MigrateLocal re-reads the session file itself, so a tiny window remains
	// between this check and that read. Closing it is not the point — the
	// precondition exists to stop a phone acting on ten-minute-old data, not to
	// make the migration a transaction.
	if wantSession != "" {
		if _, _, refusal := s.resolveLivePID(pid, wantSession); refusal != nil {
			writeJSON(w, http.StatusOK, *refusal)
			return
		}
	}
	tname, err := MigrateLocalAttested(pid, wantSession)
	if err != nil {
		result := actionResult{Error: err.Error()}
		// The re-read inside MigrateLocalAttested is the second of the two
		// identity checks; a client should not have to tell them apart by
		// matching on prose.
		if errors.Is(err, errMigrateSessionMismatch) {
			result.Code = codeSessionMismatch
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, actionResult{OK: true, Tmux: tname})
}

func (s *server) newSession(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		CWD     string `json:"cwd"`
		Name    string `json:"name"`
		Command string `json:"command"` // preset name, never raw command text
		Prompt  string `json:"prompt"`  // free text; shell-quoted before use, never interpreted
		// RequestID is an optional idempotency key. Absent, this endpoint keeps
		// its old behaviour and every call spawns. Present, a repeat of the same
		// id joins the first call if it is still running and replays its result
		// if it already succeeded — the case that actually happens is a phone
		// that timed out at 30s while the spawn was still going, and a user who
		// tapped again.
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.CWD == "" {
		http.Error(w, "cwd required", http.StatusBadRequest)
		return
	}
	if body.RequestID != "" && !validSpawnRequestID(body.RequestID) {
		http.Error(w, "bad request_id", http.StatusBadRequest)
		return
	}
	if body.RequestID == "" {
		writeJSON(w, http.StatusOK, s.spawnSession(body.CWD, body.Name, body.Command, body.Prompt, ""))
		return
	}
	flight, claim := s.spawns.begin(body.RequestID)
	switch claim {
	case spawnClaimRefused:
		// 503 rather than the 200 envelope: this is the one failure here that is
		// worth retrying unchanged, and a status says so to every client without
		// needing a third code.
		writeJSON(w, http.StatusServiceUnavailable, actionResult{
			Error: "too many spawns in flight on this host; retry with the same request_id",
		})
		return
	case spawnJoined:
		select {
		case <-flight.done:
			writeJSON(w, http.StatusOK, flight.result)
		case <-r.Context().Done():
			// The client gave up. Its spawn is still running and still owns the
			// id, so a retry will join or replay it — but this goroutine must not
			// stay parked on a hung tmux for as long as it hangs.
		}
		return
	}

	// Published through a defer so a panic in the spawn path cannot leave every
	// joiner parked on a channel that never closes. The zero value here is what
	// they would receive in that case: a failure, which the ledger then forgets,
	// so a retry genuinely re-runs.
	result := actionResult{Error: "spawn did not complete"}
	published := false
	publish := func() {
		if published {
			return
		}
		published = true
		s.spawns.finish(body.RequestID, flight, result)
	}
	// Deferred for the panic path, but called explicitly the moment the outcome
	// is known: publishing after writeJSON would let a client that is slow to
	// read hold a finished spawn's slot, and a later request would then be
	// refused for capacity nothing is actually using.
	defer publish()
	result = s.spawnSession(body.CWD, body.Name, body.Command, body.Prompt, body.RequestID)
	publish()
	writeJSON(w, http.StatusOK, result)
}

// spawnSession is newSession's work without the HTTP or the idempotency:
// validate the directory and the preset, spawn, and return the envelope the
// handler writes. Split out so the request_id dedupe wraps exactly one call to
// it, which is what keeps the side effects here — the cache invalidation and the
// trust-prompt dismissal — from running twice for one request id.
func (s *server) spawnSession(cwd, name, command, prompt, requestID string) actionResult {
	cwd = expandTilde(cwd)
	if !isDir(cwd) {
		return actionResult{Error: "not a directory: " + cwd}
	}
	presets, err := LoadCommandPresets()
	if err != nil {
		return actionResult{Error: err.Error()}
	}
	// LoadCommandPresets always yields a non-empty slice (falls back to the
	// default Claude preset), so presets[0] is a safe backward-compatible
	// default for clients that omit command. A named command must match this
	// server's own allowlist — raw command text is never accepted.
	preset := presets[0]
	if command != "" {
		var ok bool
		preset, ok = findCommandPreset(presets, command)
		if !ok {
			return actionResult{Error: "command preset not configured: " + command}
		}
	}
	launch := preset.Command
	if prompt != "" {
		launch = launch + " " + shellQuote(prompt)
	}
	tname, err := s.spawnNew(cwd, name, launch, spawnSuffix(requestID))
	if err != nil {
		return actionResult{Error: err.Error()}
	}
	if prompt != "" {
		// The client won't attach to this session, so nobody's there to accept
		// a first-run workspace trust dialog for cwd — dismiss it here if it
		// shows, without blocking the response on the poll.
		go dismissTrustPrompt(tname)
	}
	s.invalidateSessions()
	return actionResult{OK: true, Tmux: tname}
}

// registerDevice records an APNs device token for push delivery.
// POST /devices {"device_token": "...", "environment": "...", "platform": "..."}
//
// Upsert-by-token, so the app can (and should) re-register on every launch:
// APNs tokens change on restore, reinstall, and some OS upgrades, and a
// registration treated as permanent becomes a phone that silently stops
// receiving alerts.
func (s *server) registerDevice(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if s.devices == nil {
		http.Error(w, "notifications not configured on this host", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		DeviceToken string `json:"device_token"`
		Platform    string `json:"platform"`
		Environment string `json:"environment"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "unexpected trailing json", http.StatusBadRequest)
		return
	}
	// APNs device tokens are hex. Validating the shape here keeps junk out of
	// the registry, where it would otherwise sit forever failing to deliver.
	if !isAPNsDeviceToken(body.DeviceToken) {
		http.Error(w, "device_token must be 64-200 hex characters", http.StatusBadRequest)
		return
	}
	switch body.Environment {
	case "", "production", "sandbox":
	default:
		http.Error(w, "environment must be production or sandbox", http.StatusBadRequest)
		return
	}
	switch body.Platform {
	case "", "ios":
	default:
		http.Error(w, "platform must be ios", http.StatusBadRequest)
		return
	}
	// The registry is unbounded otherwise: anything holding the bearer token
	// could grow it without limit, and every entry costs a push per alert.
	if len(s.devices.List()) >= maxRegisteredDevices && !s.devices.Has(body.DeviceToken) {
		http.Error(w, "too many registered devices", http.StatusConflict)
		return
	}
	s.devices.Upsert(Device{
		Token:       body.DeviceToken,
		Platform:    body.Platform,
		Environment: body.Environment,
	})
	w.WriteHeader(http.StatusNoContent)
}

// unregisterDevice drops a device token. DELETE /devices/{token}
func (s *server) unregisterDevice(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if s.devices == nil {
		http.Error(w, "notifications not configured on this host", http.StatusServiceUnavailable)
		return
	}
	s.devices.Remove(r.PathValue("token"))
	w.WriteHeader(http.StatusNoContent)
}

// serverTokenPath is the on-disk location of the shared bearer token, or "" if
// there's no home directory.
func serverTokenPath() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "server-token")
}

// readServerToken reads the existing server token without creating one. Used by
// same-host tooling (clip-request) that must not mint a token the running
// server never loaded.
func readServerToken() (string, error) {
	path := serverTokenPath()
	if path == "" {
		return "", fmt.Errorf("no home directory")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return tok, nil
}

// loadOrCreateToken reads ~/.config/claude-sessions/server-token, creating it
// (0600) with a random value if missing. Returns the token on stdout for the
// admin to copy to client config.
func loadOrCreateToken() (string, error) {
	dir := ConfigDir()
	if dir == "" {
		return "", fmt.Errorf("no home directory")
	}
	path := serverTokenPath()
	if data, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", fmt.Errorf("%s exists but is empty; delete it to regenerate", path)
		}
		return tok, nil
	}
	b := make([]byte, 18) // 24 base64url chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// tailscaleIPv4 returns the host's Tailscale IPv4 address (or "" if Tailscale
// isn't installed/connected). Defense-in-depth alongside the bearer token.
func tailscaleIPv4() string {
	return tailscaleIPv4Context(context.Background())
}

// tailscaleIPv4Context is the context-bounded variant used by local client
// fallback, so address resolution cannot outlive its total operation deadline.
func tailscaleIPv4Context(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		s := strings.TrimSpace(line)
		if s != "" {
			return s
		}
	}
	return ""
}

// shortHostname returns hostname without the domain suffix.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	if i := strings.Index(h, "."); i >= 0 {
		h = h[:i]
	}
	// A zero-length hostname is permitted (a UTS namespace can set one), and it
	// reaches the pairing QR as a missing trailing field. Callers treat this as
	// a display name, so give them something rather than nothing.
	if h == "" {
		return "unknown"
	}
	return h
}

// serverFlags are the flags shared by `-s` and `service install`. Both parse
// them through parseServerFlags so a flag added to one can't go missing from
// the unit file the other writes.
type serverFlags struct {
	port int
	bind string
}

// parseServerFlags reads --port/--bind. An unrecognized flag is an error rather
// than a silent no-op, so a typo never reads as "use the default".
func parseServerFlags(args []string) (serverFlags, error) {
	f := serverFlags{port: defaultServerPort, bind: "127.0.0.1"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--port needs a value")
			}
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				return f, fmt.Errorf("bad port %q", args[i+1])
			}
			f.port = p
			i++
		case "--bind":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--bind needs a value")
			}
			f.bind = args[i+1]
			i++
		default:
			return f, fmt.Errorf("unknown arg %q", args[i])
		}
	}
	return f, nil
}

// unbracket strips one layer of IPv6 literal brackets, so `::` and `[::]` are
// the same host to everything downstream.
func unbracket(host string) string {
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

// hostPort joins a bind host and port into a listen address. Plain
// concatenation turns `--bind ::` into `:::8765`, which is not the address the
// user asked for; net.JoinHostPort brackets the literal properly.
func hostPort(host string, port int) string {
	return net.JoinHostPort(unbracket(host), strconv.Itoa(port))
}

// cmdServer is the -s subcommand: starts the HTTP server in the foreground.
//
// Default bind is 127.0.0.1 (safe). For remote access:
//
//	--bind tailscale    auto-detect this host's Tailscale IPv4
//	--bind 0.0.0.0      every interface (not recommended)
//	--bind <addr>       any explicit address
func cmdServer(args []string) int {
	flags, err := parseServerFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		return 2
	}
	port, bind := flags.port, flags.bind

	// Magic value: resolve "tailscale" to this host's Tailscale IPv4.
	if bind == "tailscale" {
		ts := tailscaleIPv4()
		if ts == "" {
			fmt.Fprintln(os.Stderr, "server: --bind tailscale requested but no Tailscale IPv4 found")
			fmt.Fprintln(os.Stderr, "        is tailscaled running and authenticated?")
			return 1
		}
		bind = ts
	}

	tok, err := loadOrCreateToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		return 1
	}
	host := shortHostname()

	bindHint := ""
	if bind == "127.0.0.1" || bind == "localhost" {
		bindHint = "  " + dim("(loopback — pass --bind tailscale or --bind 0.0.0.0 for remote access)")
	}

	fmt.Printf(`claude-sessions server
  bind:     %s:%d%s
  hostname: %s
  token:    %s

add to client's ~/.config/claude-sessions/servers.yaml:
  servers:
    - name: %s
      host: %s
      port: %d
      token: %s

`, bind, port, bindHint, host, tok, host, bind, port, tok)

	hostUsageHub := NewHostUsageHub(hostUsageInterval)
	defer hostUsageHub.Shutdown()

	// Account rate-limit usage: the same background poller the client uses, so a
	// remote host can surface its own account's limits (which may differ from the
	// client's account) in the client's header. The login email is read once at
	// startup — it's stable for the process's lifetime.
	usageHub := NewUsageHub()
	defer usageHub.Shutdown()

	// Codex account usage: same background poller, so a remote host also surfaces
	// its own Codex account's limits. Both snapshots are account-paired at fetch
	// time (the Anthropic email is re-read each fetch, the Codex email rides in
	// its payload), so a mid-run relogin re-attributes the limits and no separate
	// startup identity read is needed.
	codexUsageHub := NewCodexUsageHub()
	defer codexUsageHub.Shutdown()

	// The registry is shared: the /devices handlers write it and the push hub
	// reads it, so they must be the same store, not two views of one file.
	devices := LoadDeviceStore()

	s := &server{
		token:              tok,
		host:               host,
		hostSnapshot:       hostUsageHub.Snapshot,
		usageSnapshot:      usageHub.Snapshot,
		codexUsageSnapshot: codexUsageHub.Snapshot,
		devices:            devices,
	}

	// Push notifications are optional. Every failure here logs one line and
	// leaves the rest of the server untouched — a host without an APNs key runs
	// exactly as it always has.
	if cfg, err := LoadAPNsConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "push notifications disabled (%v)\n", err)
	} else if client, err := newAPNsClient(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "push notifications disabled (%v)\n", err)
	} else {
		notifier := newNotifyHub(notifyHubOptions{
			HostName: host,
			HostID:   LoadHostID(),
			BundleID: cfg.BundleID,
			Devices:  devices,
			Sender:   client,
		})
		notifier.Start()
		defer notifier.Shutdown()
		fmt.Fprintf(os.Stderr, "push notifications enabled (%s, %d device(s))\n",
			cfg.Environment, len(devices.List()))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", s.sessions)
	mux.HandleFunc("GET /cwd-suggestions", s.cwdSuggestions)
	mux.HandleFunc("GET /presets", s.presets)
	mux.HandleFunc("GET /resumable", s.resumable)
	mux.HandleFunc("POST /sessions/resume", s.resume)
	mux.HandleFunc("GET /sessions/{pid}/preview", s.preview)
	mux.HandleFunc("GET /sessions/{pid}/tmux-info", s.tmuxInfo)
	mux.HandleFunc("POST /sessions/{pid}/kill", s.kill)
	mux.HandleFunc("POST /sessions/{pid}/migrate", s.migrate)
	mux.HandleFunc("POST /sessions/new", s.newSession)
	mux.HandleFunc("POST /worktree/remove", s.removeWorktree)
	mux.HandleFunc("POST /devices", s.registerDevice)
	mux.HandleFunc("DELETE /devices/{token}", s.unregisterDevice)
	mux.HandleFunc("POST /pair/arm", s.armPairing)
	mux.HandleFunc("POST /pair/disarm", s.disarmPairing)
	mux.HandleFunc("POST /pair/exchange", s.pairExchange)
	mux.HandleFunc("GET /paste-wait", s.pasteWait)
	mux.HandleFunc("POST /paste-request", s.pasteRequest)
	mux.HandleFunc("POST /paste", s.pasteUpload)

	// Publish the resolved port so SpawnNew (invoked without it) embeds the right
	// port in the tmux paste binding. Intercept Ctrl+V in tmux so remote-image
	// paste works, and drop any paste temp files left behind by an earlier run.
	// Both are linux-only no-ops elsewhere. Re-assert the binding periodically in
	// case the tmux server was restarted (or first started) after us.
	activeServerPort = port
	installPasteBinding(port)
	gcOldPastes(time.Now(), pasteGCMaxAge)
	if runtime.GOOS == "linux" {
		go func() {
			t := time.NewTicker(60 * time.Second)
			defer t.Stop()
			for range t.C {
				installPasteBinding(port)
			}
		}()
	}

	// A non-loopback bind leaves clip-request's same-host POST with nothing to
	// dial, so serve /paste-request on loopback as well. Best-effort: a failure
	// here costs remote image paste, not the server.
	if lb := loopbackPasteAddr(bind, port); lb != "" {
		if ln, err := listenLoopbackPaste(lb, s); err != nil {
			fmt.Fprintf(os.Stderr, "remote image paste disabled (%s: %v)\n", lb, err)
		} else {
			defer ln.Close()
			fmt.Fprintf(os.Stderr, "paste requests also on %s\n", lb)
		}
	}

	addr := hostPort(bind, port)
	fmt.Fprintf(os.Stderr, "listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		return 1
	}
	return 0
}
