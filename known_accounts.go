package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// KnownAccountUsage is one non-live known account's rate-limit snapshot, read
// from its claude-switch credential snapshot rather than the host's active
// login.
//
// Four states, not two, because collapsing them was actively misleading: a 429
// from the shared per-token budget used to render as "auth expired" and send
// the user off to re-login over a throttle that heals itself in seconds.
//
//	Info != nil, !Stale          fresh numbers
//	Info != nil, Stale           numbers carried forward from an earlier poll,
//	                             kept because this attempt failed transiently
//	Expired                      401/403 — the credential really is dead, and
//	                             no numbers are carried forward beside it
//	Info == nil, Reason != ""    transient failure with nothing to carry
//
// Reason is the classification tag from classifyUsageErr, never raw error text.
type KnownAccountUsage struct {
	Name    string     `json:"name"`    // snapshot name, e.g. "avisoma"
	Account string     `json:"account"` // email, "" if account.json missing/unreadable
	Info    *UsageInfo `json:"info"`    // fresh, or (with Stale) carried forward; nil when Expired
	Expired bool       `json:"expired"` // a genuine 401/403 only — the actionable "re-login" state
	Stale   bool       `json:"stale,omitempty"`
	// Reason is set whenever the latest attempt failed: alongside Expired, and
	// alongside either carried-forward or absent numbers. Empty on a clean fetch.
	Reason string `json:"reason,omitempty"`
	// FetchedAt is when Info was actually fetched, not when this struct was
	// built — a carried-forward entry keeps the original timestamp, which is
	// what bounds how long staleness may accumulate (usageCacheMaxAge).
	//
	// omitzero, not omitempty: encoding/json never considers a struct empty, so
	// omitempty on a time.Time does nothing and every entry with no numbers
	// would ship a "0001-01-01T00:00:00Z".
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
}

// snapshotCredentialSuffix is the fixed tail claude-switch gives a stashed
// credential file: on macOS the live credential is a Keychain item, so a
// switched-away account is parked in a plain ~/.claude/.<name>.keychain-cred
// file holding the same JSON blob `security find-generic-password -w` prints;
// elsewhere it's a copy of ~/.claude/.credentials.json named
// ~/.claude/.<name>.credentials.json. Neither can be confused with the live
// file on its own platform — the live name has no <name> segment, so it is
// exactly the suffix with a leading dot and nothing between the two, which the
// length check in snapshotAccountNames rejects (verified by
// TestSnapshotAccountNames).
func snapshotCredentialSuffix() string {
	if runtime.GOOS == "darwin" {
		return ".keychain-cred"
	}
	return ".credentials.json"
}

// snapshotCredentialPath is the credential-snapshot file for one account name
// under home. Tests build fixtures through it so they stay platform-agnostic.
func snapshotCredentialPath(home, name string) string {
	return filepath.Join(home, ".claude", "."+name+snapshotCredentialSuffix())
}

// snapshotAccountPath is the account-metadata file claude-switch writes beside
// the credential snapshot; it carries the account's email.
func snapshotAccountPath(home, name string) string {
	return filepath.Join(home, ".claude", "."+name+".account.json")
}

// snapshotAccountNames lists the claude-switch credential snapshots this host
// holds, in directory (lexical) order. Names come from the file names alone —
// the platform-specific fixed parts are stripped — so no new config is needed
// and nothing is read until a caller asks for a specific account.
//
// The listing is a plain os.ReadDir + name filter rather than filepath.Glob on
// purpose: the home directory is data, not a pattern, and a home path holding a
// glob metacharacter would otherwise either fail the whole batch (an unmatched
// "[" is ErrBadPattern, the poller's only error path — one bad path and every
// account silently disappears into permanent backoff) or match somewhere else
// entirely (a "*" in the path globs across sibling directories, reading other
// directories' credential snapshots).
//
// Only an unresolvable home directory errors. A host with no snapshots — no
// ~/.claude at all, an empty one, or one this process cannot read — yields an
// empty list, which is what keeps a host-level oddity from putting the poller
// into backoff and hiding every account.
func snapshotAccountNames() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude"))
	if err != nil {
		return nil, nil
	}
	suffix := snapshotCredentialSuffix()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		base := e.Name()
		// A leading dot, the platform's suffix, and at least one character of
		// <name> between them — which is exactly what the live credential file
		// (the bare suffix with a leading dot) lacks.
		if !strings.HasPrefix(base, ".") || !strings.HasSuffix(base, suffix) || len(base) <= 1+len(suffix) {
			continue
		}
		name := base[1 : len(base)-len(suffix)]
		// The rescue slot has the same file-name shape as a snapshot but is not
		// an account: it is the rolling backup of whatever credential was live
		// at the last switch (see account.go), owned jointly with the
		// claude-switch shell script, with no identity file and nobody logged
		// into it. Excluding it here keeps it out of the usage poller, `account
		// list`, and the Ctrl+W picker in one place.
		if name == rescueSnapshotName {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// snapshotToken reads one snapshot's OAuth access token, parsed exactly the way
// the live credential is (parseOAuthCredentials). Strictly read-only: nothing
// here writes a credential file or touches the Keychain, so the account that is
// actually logged in on this host is never disturbed.
func snapshotToken(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(snapshotCredentialPath(home, name))
	if err != nil {
		return "", err
	}
	return parseOAuthCredentials(data)
}

// snapshotAccountEmail reads a snapshot's login email from
// ~/.claude/.<name>.account.json, mirroring loadAccountEmail's read of the live
// ~/.claude.json (same oauthAccount.emailAddress shape). Returns "" on any
// error — an unknown email is a display detail, never a failure.
func snapshotAccountEmail(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(snapshotAccountPath(home, name))
	if err != nil {
		return ""
	}
	var raw struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	return raw.OAuthAccount.EmailAddress
}

// emailMatchesLive reports whether a snapshot's stored email stands for the
// account currently logged in on this host. Matching is by email equality — the
// email is already in .<name>.account.json, so no domain-to-name mapping is
// needed in Go (unlike claude-switch's shell domain_for). An unknown email on
// either side never matches: with no evidence, fetching the snapshot separately
// is the safe answer (a duplicate line beats a silently dropped account).
//
// It is the single comparison behind both things the pass produces: which
// snapshot to skip, and (the same decision, reported outward) which snapshot
// name stands for the live account.
func emailMatchesLive(snapEmail, liveEmail string) bool {
	return snapEmail != "" && liveEmail != "" && strings.EqualFold(snapEmail, liveEmail)
}

// fetchKnownAccountUsage fetches one snapshot account's rate limits with that
// snapshot's own stored token. The bool reports whether this name was skipped
// as covering the live account — the live account is always fetched through the
// live path instead, because Claude Code rotates the live token in place while
// the snapshot copy keeps whatever claude-switch stashed at switch time, so the
// snapshot can read as expired while the live session is perfectly healthy.
//
// No read/parse/HTTP error is ever returned as an error: this is best-effort
// background enrichment and must never be a reason to fail /sessions or the
// poller. A failure becomes that entry's classification instead (see
// knownAccountUsage), and prev — this account's last result, nil when there is
// none — is what a transient failure carries forward.
func fetchKnownAccountUsage(name, liveEmail string, prev *KnownAccountUsage) (*KnownAccountUsage, bool) {
	return knownAccountUsage(name, liveEmail, prev, usageInfoFetch)
}

// usageInfoFetch is fetchUsageInfo behind a package var so tests can drive the
// poller's own fetch — the one place carrying numbers across polls is decided —
// without a network or a real token. Never reassigned outside tests; the same
// seam keychainRead/keychainWrite use in account.go.
var usageInfoFetch = fetchUsageInfo

// usageBadSnapshotReason is the classification for a credential snapshot that
// could not be read or parsed at all. It is deliberately not one of
// classifyUsageErr's tags: those describe what the endpoint (or the network)
// answered, and this failure never got that far. The wording points at the file
// because that is the only place the fix lives.
const usageBadSnapshotReason = "bad snapshot"

// knownAccountUsage is fetchKnownAccountUsage with the HTTP leg injected, so
// tests exercise every branch without a network or a real token.
//
// A failure splits three ways. A genuine 401/403 is Expired and carries NO
// numbers forward — the credential is dead, and bars beside a dead credential
// would imply it still works. Any other failure (429, 5xx, timeout, network, an
// unreadable credential file) re-serves prev's numbers marked Stale, with
// prev's ORIGINAL FetchedAt: restamping it to now would let an account that
// fails forever look permanently fresh, when the whole point of the timestamp
// is to stop carrying numbers past usageCacheMaxAge. With nothing to carry, the
// entry keeps only its identity and the reason.
func knownAccountUsage(name, liveEmail string, prev *KnownAccountUsage, fetch func(token string) (*UsageInfo, error)) (*KnownAccountUsage, bool) {
	email := snapshotAccountEmail(name)
	if emailMatchesLive(email, liveEmail) {
		return nil, true
	}
	failed := func(expired bool, reason string) (*KnownAccountUsage, bool) {
		if expired {
			return &KnownAccountUsage{Name: name, Account: email, Expired: true, Reason: reason}, false
		}
		if carryable(prev, email) {
			return &KnownAccountUsage{
				Name:      name,
				Account:   email,
				Info:      prev.Info,
				Stale:     true,
				Reason:    reason,
				FetchedAt: prev.FetchedAt,
			}, false
		}
		return &KnownAccountUsage{Name: name, Account: email, Reason: reason}, false
	}
	tok, err := snapshotToken(name)
	if err != nil {
		// Not an HTTP answer at all — a missing, unreadable or unparseable
		// credential snapshot says nothing about whether the token it should
		// hold is still good, so it never means Expired. It gets its own tag
		// rather than sharing "unreachable": nothing here is a network problem,
		// and unlike the failures that heal on their own this one stays until
		// the snapshot is rewritten (`account save <name>` while logged into
		// it, the same recovery switchAccount's identity precondition names).
		return failed(false, usageBadSnapshotReason)
	}
	info, err := fetch(tok)
	if err != nil {
		return failed(classifyUsageErr(err))
	}
	return &KnownAccountUsage{Name: name, Account: email, Info: info, FetchedAt: time.Now()}, false
}

// fresh reports whether a result's own numbers are young enough to still
// inform rather than mislead (the same bound a warm start from disk obeys),
// with no identity check — it is used only where prev and the value being
// aged are the same record (loadKnownAccountsCache checking a cache entry
// against itself), so there is nothing to misattribute.
func fresh(v *KnownAccountUsage) bool {
	return v != nil && v.Info != nil && !v.FetchedAt.IsZero() &&
		time.Since(v.FetchedAt) <= usageCacheMaxAge
}

// carryable reports whether a previous result's numbers may be re-served
// under a DIFFERENT attempt's email: fresh (see above), plus, critically,
// still the SAME account. prev is keyed by snapshot name in
// newKnownAccountsFetcher's memory, not by account identity, so a name that
// gets reused for a different account (`account save work` while logged into
// an account that previously belonged to someone else) must not carry the old
// account's numbers forward under the new account's email — that would
// misattribute usage across accounts, which is worse than the over-broad
// "auth expired" this whole mechanism replaced. email is the name's
// freshly-read current account (snapshotAccountEmail, read moments before
// this call); an unknown prev or current email (either "") never carries,
// since identity can't be confirmed either way.
func carryable(prev *KnownAccountUsage, email string) bool {
	return fresh(prev) && prev.Account != "" && email != "" && strings.EqualFold(prev.Account, email)
}

// fetchAllKnownAccounts fetches every named snapshot account in parallel (small,
// fixed fanout — one goroutine per account claude-switch knows about), skipping
// whichever name stands for liveEmail, and returns the survivors in names order
// plus that skipped name (the active snapshot; "" when none matched).
//
// It never returns an error, and never omits an account because that account
// failed: a failure is carried as that entry's own classification. That is the
// property KnownAccountsHub depends on — the poller's fetch only fails for a
// catastrophic host-level problem, so one flaky account can't trigger the
// whole-batch backoff and hide every other account's healthy data.
//
// prev holds each account's last result, keyed by snapshot name (absent for an
// account that has none yet); it is what a transient failure re-serves.
func fetchAllKnownAccounts(names []string, liveEmail string, prev map[string]KnownAccountUsage) ([]KnownAccountUsage, string) {
	return allKnownAccounts(names, liveEmail, prev, fetchKnownAccountUsage)
}

// allKnownAccounts is fetchAllKnownAccounts with the per-account fetch injected.
// The active-snapshot name is a genuine side output of this one pass — it is the
// name whose own email/live-email comparison came back "skip", so it can never
// disagree with which account the pass actually left out, the way an independent
// second lookup (different call, possibly a different live email) could.
func allKnownAccounts(names []string, liveEmail string, prev map[string]KnownAccountUsage, one func(name, liveEmail string, prev *KnownAccountUsage) (*KnownAccountUsage, bool)) ([]KnownAccountUsage, string) {
	results := make([]*KnownAccountUsage, len(names))
	// Per-index, never a shared variable: the goroutines run concurrently, and
	// the scan below wants names order anyway.
	skipped := make([]bool, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		i, name := i, name
		// Indexed here rather than inside the goroutine, so nothing concurrent
		// ever touches the caller's map.
		var last *KnownAccountUsage
		if p, ok := prev[name]; ok {
			p := p
			last = &p
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], skipped[i] = one(name, liveEmail, last)
		}()
	}
	wg.Wait()
	out := make([]KnownAccountUsage, 0, len(names))
	active := ""
	for i, r := range results {
		if skipped[i] && active == "" {
			active = names[i]
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, active
}

// knownAccountsResult is one poll's outcome: usage for every known account this
// host is not logged into, plus the snapshot name that stood for the live
// account in that same pass. They travel together so the server answers
// "activeSnapshotName" from a value the poller already resolved instead of
// re-listing snapshots and re-reading every .<name>.account.json on each
// /sessions request.
type knownAccountsResult struct {
	Accounts []KnownAccountUsage
	// ActiveName is best-effort and read-only: "" when the live email is
	// unknown or no snapshot matches it.
	ActiveName string
}

// newKnownAccountsFetcher builds the poller's fetch: resolve this host's
// snapshot names, then fetch every one that isn't the live account. The live
// email is re-read per fetch, like fetchUsage does, so a relogin mid-run
// immediately changes both which snapshot is skipped and which one is reported
// as active. Only an unresolvable home directory is an error; zero snapshots is
// an empty list.
//
// It is a closure rather than a plain function because carrying numbers across
// a failed poll needs memory of the previous one, and usagePoller's fetch takes
// no arguments. seed is the disk cache the hub also seeds the poller with (nil
// when there is none), so a restart mid-throttle can carry forward too instead
// of starting from nothing.
//
// last is rebuilt from each pass rather than mutated, and only from entries
// that actually have numbers: an account whose snapshot was renamed or deleted
// drops out of names, so it drops out of last on the next pass instead of
// lingering for the life of the process.
func newKnownAccountsFetcher(seed *knownAccountsResult) func() (*knownAccountsResult, error) {
	var mu sync.Mutex
	last := map[string]KnownAccountUsage{}
	if seed != nil {
		for _, a := range seed.Accounts {
			last[a.Name] = a
		}
	}
	return func() (*knownAccountsResult, error) {
		names, err := snapshotAccountNames()
		if err != nil {
			return nil, err
		}
		mu.Lock()
		prev := make(map[string]KnownAccountUsage, len(names))
		for _, n := range names {
			if a, ok := last[n]; ok {
				prev[n] = a
			}
		}
		mu.Unlock()
		accounts, active := fetchAllKnownAccounts(names, loadAccountEmail(), prev)
		fresh := make(map[string]KnownAccountUsage, len(accounts))
		for _, a := range accounts {
			if a.Info != nil {
				fresh[a.Name] = a
			}
		}
		mu.Lock()
		last = fresh
		mu.Unlock()
		return &knownAccountsResult{Accounts: accounts, ActiveName: active}, nil
	}
}

// knownAccountsCachePath is where the last successful known-accounts fetch is
// persisted so a restart during an endpoint throttle still has something to
// show. Separate file from the live-usage cache; UID in the name keeps
// multi-user /tmp collisions away.
func knownAccountsCachePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("claude-sessions-known-accounts-%d.json", os.Getuid()))
}

// cachedKnownAccounts is the on-disk envelope: the per-account snapshots plus
// when the file was written. That envelope timestamp is informational only —
// each entry carries its own FetchedAt, and an entry re-served after a failed
// refresh keeps the timestamp of the fetch that actually produced its numbers,
// so an envelope-level "written just now" would say nothing about how old the
// numbers inside are. The active-snapshot name is deliberately not persisted —
// it describes which account is logged in *now*, which a restart can have
// changed, and it is re-resolved by the first fetch anyway.
type cachedKnownAccounts struct {
	FetchedAt time.Time           `json:"fetched_at"`
	Accounts  []KnownAccountUsage `json:"accounts"`
}

// saveKnownAccountsCache persists the entries of a fetch that have numbers —
// expired ones are deliberately dropped, so a restart can seed a recently-good
// account but never resurrects a previously-expired one as if it were fresh (it
// starts as "no data yet" and waits for the next live fetch to decide). A stale
// entry does pass, carrying the FetchedAt of the fetch its numbers came from,
// which is what stops a restart from laundering it into a fresh reading.
// Best-effort: a read-only /tmp just means no warm start next launch.
func saveKnownAccountsCache(res *knownAccountsResult) {
	fresh := make([]KnownAccountUsage, 0, len(res.Accounts))
	for _, a := range res.Accounts {
		if a.Expired || a.Info == nil {
			continue
		}
		fresh = append(fresh, a)
	}
	data, err := json.Marshal(cachedKnownAccounts{FetchedAt: time.Now(), Accounts: fresh})
	if err != nil {
		return
	}
	_ = os.WriteFile(knownAccountsCachePath(), data, 0600)
}

// loadKnownAccountsCache returns the cached snapshots, or nil if absent,
// unreadable, or carrying nothing usable. An entry without Info is dropped
// rather than seeded: dedupeAccounts would silently discard it anyway, and it
// must not masquerade as a healthy account. A seeded result carries no
// ActiveName (see cachedKnownAccounts) — the first fetch fills it in.
//
// Age is judged per account, not per file, because entries in one file no
// longer share an age: a stale entry's numbers can be usageCacheMaxAge older
// than the write that persisted them. carryable is the same gate a live
// carry-forward uses, so an entry that survives a restart is exactly one that
// would have survived without one. A cache file written before per-account
// timestamps existed decodes to the zero time and is therefore dropped whole —
// intended: one poll replaces it, and inventing an age for numbers of unknown
// vintage is what this gate exists to prevent.
func loadKnownAccountsCache() *knownAccountsResult {
	data, err := os.ReadFile(knownAccountsCachePath())
	if err != nil {
		return nil
	}
	var c cachedKnownAccounts
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	accounts := make([]KnownAccountUsage, 0, len(c.Accounts))
	for _, a := range c.Accounts {
		a := a
		if a.Expired || !fresh(&a) {
			continue
		}
		accounts = append(accounts, a)
	}
	if len(accounts) == 0 {
		return nil
	}
	return &knownAccountsResult{Accounts: accounts}
}

// KnownAccountsHub polls every account this host holds a claude-switch
// credential snapshot for, alongside UsageHub's poll of the live account (see
// usagePoller for the shared mechanism: same 2-minute cadence, same
// failed-fetch backoff, same pause/resume/kick surface). Runs in both roles —
// on the server for remote hosts, and locally for the client's own snapshots.
type KnownAccountsHub = usagePoller[knownAccountsResult]

// NewKnownAccountsHub starts the poller and returns immediately, seeded from a
// recent disk cache. The cache is read once and handed to both the poller (what
// the header shows until the first fetch lands) and the fetcher (what a first
// pass that fails transiently carries forward) — two reads could disagree, and
// the second would buy nothing.
func NewKnownAccountsHub() *KnownAccountsHub {
	seed := loadKnownAccountsCache()
	return newUsagePoller(seed, newKnownAccountsFetcher(seed), saveKnownAccountsCache)
}

// derefKnownAccounts flattens the poller's *knownAccountsResult (nil until the
// first fetch lands) into the plain slice the renderer takes.
func derefKnownAccounts(res *knownAccountsResult) []KnownAccountUsage {
	if res == nil {
		return nil
	}
	return res.Accounts
}

// activeSnapshotNameOf is derefKnownAccounts' counterpart for the active
// snapshot name: "" until the first fetch lands, or when nothing matched.
func activeSnapshotNameOf(res *knownAccountsResult) string {
	if res == nil {
		return ""
	}
	return res.ActiveName
}
