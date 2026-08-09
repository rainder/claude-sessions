package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	// Verified records that Info's identity was confirmed by the profile probe —
	// this token really does belong to Account — rather than merely assumed from
	// the snapshot's own .<name>.account.json.
	//
	// It exists for one narrow but load-bearing rule: only a *verified* reading
	// may be carried forward past a `wrong identity` failure. Without it, a pass
	// whose probe was unavailable stores a stranger's numbers under the claimed
	// email (nothing contradicted the claim, so the entry looks ordinary), and
	// the next pass — which does reach the probe and does detect the
	// misattribution — carries exactly those numbers forward through carryable,
	// whose identity test only compares the *claimed* email on both sides and so
	// cannot tell them apart. That is the one thing the wrong-identity
	// classification exists to prevent, reached through the carry instead of
	// through the fresh reading.
	//
	// Serialized so a disk-cache warm start keeps the distinction; a cache
	// written before this field existed decodes to false, which is the safe
	// direction (it declines a carry it might have allowed).
	Verified bool `json:"verified,omitempty"`
	// FetchedAt is when Info was actually fetched, not when this struct was
	// built — a carried-forward entry keeps the original timestamp rather than
	// being restamped on every re-serve. Nothing in this TUI's own header
	// renders it (the Stale marker is a fixed "stale" label, not a computed
	// age), but it still travels on the wire to any other consumer, and
	// restamping it would make a permanently throttled account's numbers look
	// permanently fresh there too.
	//
	// omitzero, not omitempty: encoding/json never considers a struct empty, so
	// omitempty on a time.Time does nothing and every entry with no numbers
	// would ship a "0001-01-01T00:00:00Z".
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
	// RetryAt is a 429's Retry-After, when the endpoint gave one (see
	// parseRetryAfter). Deliberately not serialized: it is scheduling state for
	// the fetcher that produced this entry — how long *it* should wait before
	// asking again — and means nothing to a consumer of the numbers.
	RetryAt time.Time `json:"-"`
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

// usageInfoFetch is the usage round trip behind a package var so tests can drive
// every fetch path — the poller's, the live account's, and the server's — without
// a network or a real token. Never reassigned outside tests; the same seam
// keychainRead/keychainWrite use in account.go.
//
// Its production value is fetchVerifiedUsageInfo rather than the bare
// fetchUsageInfo, so identity verification rides along with every fetch through
// one seam instead of three. A test fake therefore controls both halves at once:
// leaving UsageInfo.VerifiedAccount zero reproduces exactly the pre-verification
// behaviour (fall back to the file read), and setting it drives the verified
// path.
var usageInfoFetch = fetchVerifiedUsageInfo

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
// fails forever look permanently fresh to any consumer of the timestamp, even
// though this TUI's own header shows only a fixed "stale" label rather than a
// computed age. Carrying is not age-bounded — old numbers beside a visible
// Stale marker still beat no numbers at all — so this keeps working for as
// long as the account stays down. With nothing to carry, the entry keeps only
// its identity and the reason.
func knownAccountUsage(name, liveEmail string, prev *KnownAccountUsage, fetch func(token string) (*UsageInfo, error)) (*KnownAccountUsage, bool) {
	email := snapshotAccountEmail(name)
	if emailMatchesLive(email, liveEmail) {
		return nil, true
	}
	// needVerified is set only by the wrong-identity path: everything else keeps
	// the carry rules it always had, since a 429 or a network blip says nothing
	// about whose numbers prev holds — carryable's claimed-email test is the
	// right and sufficient one there. See KnownAccountUsage.Verified.
	failed := func(expired bool, reason string, needVerified bool) (*KnownAccountUsage, bool) {
		if expired {
			return &KnownAccountUsage{Name: name, Account: email, Expired: true, Reason: reason}, false
		}
		if carryable(prev, email) && (!needVerified || prev.Verified) {
			return &KnownAccountUsage{
				Name:      name,
				Account:   email,
				Info:      prev.Info,
				Stale:     true,
				Reason:    reason,
				FetchedAt: prev.FetchedAt,
				// Carried forward with its numbers: the flag describes how
				// Info's identity was established, and re-serving the same
				// Info does not weaken that. Dropping it would make a single
				// throttled pass between a good fetch and a mismatch enough to
				// re-open the hole Verified closes.
				Verified: prev.Verified,
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
		return failed(false, usageBadSnapshotReason, false)
	}
	info, err := fetch(tok)
	if err != nil {
		expired, reason := classifyUsageErr(err)
		res, skip := failed(expired, reason, false)
		// Only a throttle carries a retry hint, and only the throttle path has a
		// scheduler waiting for one (newKnownAccountsFetcher's backoff map).
		if reason == usageRateLimitedReason {
			res.RetryAt = usageRetryAt(err)
		}
		return res, skip
	}
	// The token answered, but for a different account than the snapshot's
	// identity file claims — the exact misattribution `account save` under the
	// wrong name produces, and the after-the-fact signature of a credential
	// clobbered by a still-running session of another account. Showing the
	// FRESH numbers here would put one account's budget on screen under
	// another's email, which is worse than showing none.
	//
	// It goes through failed() like every other non-expired outcome, but with the
	// one extra condition carryable cannot express: prev may only be re-served
	// here if prev's OWN identity was verified. carryable compares the claimed
	// email on both sides, and on this path the claim is exactly what is in
	// doubt — an earlier pass whose probe was unavailable would have stored the
	// stranger's numbers under this same claimed email, and carrying those
	// forward would smuggle in precisely what dropping the fresh reading
	// prevents. See KnownAccountUsage.Verified.
	//
	// An unreadable identity file (email "") claims nothing, so there is nothing
	// for the verified answer to contradict; that case keeps the behaviour it
	// always had — an entry with no email, which the header drops anyway.
	if verifiedIdentityMismatch(email, info) {
		return failed(false, usageWrongIdentityReason, true)
	}
	return &KnownAccountUsage{
		Name:      name,
		Account:   email,
		Info:      info,
		Verified:  info.VerifiedAccount != "",
		FetchedAt: time.Now(),
	}, false
}

// fresh reports whether a result actually has numbers on it — real Info and a
// timestamp establishing their vintage — with no identity check and, since
// carrying is not age-bounded, no age check either: an unstamped entry (a
// pre-timestamp disk write) is the only thing this rejects. Used where prev
// and the value being judged are the same record (loadKnownAccountsCache
// checking a cache entry against itself), so there is nothing to misattribute.
func fresh(v *KnownAccountUsage) bool {
	return v != nil && v.Info != nil && !v.FetchedAt.IsZero()
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

// allKnownAccounts fetches every named snapshot account one at a time, in the
// order given, skipping whichever name stands for liveEmail, and returns the
// survivors in that same order plus the skipped name (the active snapshot; ""
// when none matched).
//
// Sequential rather than fanned out: these are typically a handful of
// different Anthropic accounts, each with its own token and budget, so
// nothing here risks one account's throttle on another's — but they are all
// requests from the same process to the same endpoint at the same moment, and
// spreading them out in time rather than bursting them costs one pass a
// little wall-clock (bounded by the timeout inside each fetch) in exchange for
// never presenting as a burst. names is the order newKnownAccountsFetcher
// hands in, not raw snapshotAccountNames() order — see that function for how
// a name that failed (or was skipped as backed-off) this pass earns the front
// of the queue next time, so a chronically struggling account isn't
// perpetually the last one attempted.
//
// It never returns an error, and never omits an account because that account
// failed: a failure is carried as that entry's own classification. That is the
// property KnownAccountsHub depends on — the poller's fetch only fails for a
// catastrophic host-level problem, so one flaky account can't trigger the
// whole-batch backoff and hide every other account's healthy data.
//
// prev holds each account's last result, keyed by snapshot name (absent for an
// account that has none yet); it is what a transient failure re-serves.
//
// The per-account fetch is injected, which is also how the poller substitutes a
// no-round-trip answer for an account it is currently backing off (see
// newKnownAccountsFetcher). The active-snapshot name is a genuine side output of
// this one pass — it is the name whose own email/live-email comparison came back
// "skip", so it can never disagree with which account the pass actually left
// out, the way an independent second lookup (different call, possibly a
// different live email) could.
func allKnownAccounts(names []string, liveEmail string, prev map[string]KnownAccountUsage, one func(name, liveEmail string, prev *KnownAccountUsage) (*KnownAccountUsage, bool)) ([]KnownAccountUsage, string) {
	out := make([]KnownAccountUsage, 0, len(names))
	active := ""
	for _, name := range names {
		var last *KnownAccountUsage
		if p, ok := prev[name]; ok {
			p := p
			last = &p
		}
		r, skipped := one(name, liveEmail, last)
		if skipped && active == "" {
			active = name
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

// reconcileFetchOrder carries prior's relative order forward for every name
// still present in names, then appends whatever is in names but not in
// prior — a snapshot saved since the last pass — in names' own (directory)
// order at the end. A name prior remembered that no longer exists (removed or
// renamed) simply drops out.
func reconcileFetchOrder(prior, names []string) []string {
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	seen := make(map[string]bool, len(prior))
	next := make([]string, 0, len(names))
	for _, n := range prior {
		if present[n] {
			next = append(next, n)
			seen[n] = true
		}
	}
	for _, n := range names {
		if !seen[n] {
			next = append(next, n)
		}
	}
	return next
}

// reorderFailedFirst moves every name in failed to the front of order,
// preserving order's own relative order within each of the two groups.
func reorderFailedFirst(order []string, failed map[string]bool) []string {
	next := make([]string, 0, len(order))
	for _, n := range order {
		if failed[n] {
			next = append(next, n)
		}
	}
	for _, n := range order {
		if !failed[n] {
			next = append(next, n)
		}
	}
	return next
}

// newKnownAccountsFetcher builds the poller's fetch: resolve this host's
// snapshot names, then fetch every one that isn't the live account, one at a
// time (allKnownAccounts). The live email is re-read per fetch, like
// fetchUsage does, so a relogin mid-run immediately changes both which
// snapshot is skipped and which one is reported as active. Only an
// unresolvable home directory is an error; zero snapshots is an empty list.
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
//
// The closure carries a second memory beside it: how many consecutive passes
// each account has been rate limited, and when it may be tried again
// (usageBackoff). The same account is routinely held as a credential snapshot
// on more than one host, and every host polls it on its own tick, so an account
// the endpoint keeps throttling would otherwise cost a failed round trip per
// host per usageRefreshInterval forever. While an account is backed off its
// fetch is not attempted at all — the entry is synthesized from the answer a
// throttled fetch would have produced, so the header keeps showing exactly what
// it showed before, carried forward and marked stale like any other transient
// failure.
//
// A third memory is the fetch order itself. Since allKnownAccounts now goes
// one account at a time rather than fanning every account out at once, WHICH
// one goes first matters — not for whether it gets attempted this pass (every
// name in fetchOrder is: skip is decided from the backoff map alone, gating on
// due(now) rather than position) but for the order requests actually fire in.
// Any account this pass could not get fresh numbers for — a genuine failure or
// a synthesized backoff answer, `Expired || Reason != ""` either way — moves
// to the front for the next pass (reorderFailedFirst), ahead of the accounts
// that are fine, on the request explicitly made: whatever failed most
// recently is retried first. reconcileFetchOrder folds in additions and
// removals each pass so a renamed or newly-saved snapshot is never silently
// dropped from the rotation. The returned result is sorted by name regardless
// of fetchOrder (see below), so this reordering is invisible to the header
// and every other consumer — it only changes request sequence, not content.
//
// All three maps and the order slice are rebuilt or recomputed from names each
// pass, for the same reason, and all are only ever read and written here — in
// the serial closure the poller calls one tick at a time.
func newKnownAccountsFetcher(seed *knownAccountsResult) func() (*knownAccountsResult, error) {
	var mu sync.Mutex
	last := map[string]KnownAccountUsage{}
	if seed != nil {
		for _, a := range seed.Accounts {
			last[a.Name] = a
		}
	}
	backoff := map[string]usageBackoff{}
	var order []string
	return func() (*knownAccountsResult, error) {
		names, err := snapshotAccountNames()
		if err != nil {
			return nil, err
		}
		now := time.Now()
		mu.Lock()
		prev := make(map[string]KnownAccountUsage, len(names))
		held := make(map[string]usageBackoff, len(names))
		for _, n := range names {
			if a, ok := last[n]; ok {
				prev[n] = a
			}
			if b, ok := backoff[n]; ok {
				held[n] = b
			}
		}
		fetchOrder := reconcileFetchOrder(order, names)
		mu.Unlock()

		// Decided before the sequential walk so nothing in it needs to touch
		// the closure's own maps.
		skip := make(map[string]bool, len(held))
		for n, b := range held {
			if !b.due(now) {
				skip[n] = true
			}
		}
		one := func(name, liveEmail string, p *KnownAccountUsage) (*KnownAccountUsage, bool) {
			if !skip[name] {
				return fetchKnownAccountUsage(name, liveEmail, p)
			}
			// A backed-off account still goes through knownAccountUsage, with the
			// answer a throttled endpoint would have given substituted for the round
			// trip. That keeps one place deciding whether this name stands for the
			// live account (which is what resolves ActiveName, and what a name
			// filtered out of the batch entirely would have silently broken),
			// whether prev's numbers may be carried forward, and how the entry reads.
			return knownAccountUsage(name, liveEmail, p, func(string) (*UsageInfo, error) {
				return nil, &usageHTTPError{Status: http.StatusTooManyRequests}
			})
		}
		accounts, active := allKnownAccounts(fetchOrder, loadAccountEmail(), prev, one)

		fresh := make(map[string]KnownAccountUsage, len(accounts))
		listed := make(map[string]bool, len(accounts))
		failed := make(map[string]bool, len(accounts))
		for _, a := range accounts {
			listed[a.Name] = true
			if a.Info != nil {
				fresh[a.Name] = a
			}
			// Anything that isn't a clean success — Expired, or any Reason at
			// all (a genuine failure this pass, or a synthesized backoff
			// answer) — earns the front of the queue next time.
			if a.Expired || a.Reason != "" {
				failed[a.Name] = true
			}
			// A synthesized entry is not an attempt, so it neither lengthens the
			// wait nor resets it — the deadline already armed simply stands.
			if skip[a.Name] {
				continue
			}
			if a.Reason == usageRateLimitedReason {
				held[a.Name] = held[a.Name].next(now, a.RetryAt)
				continue
			}
			// Anything else — numbers, a dead credential, a failure of some other
			// kind — ends the streak. The state tracks consecutive throttles only.
			delete(held, a.Name)
		}
		// A name in names with no entry at all is the one this pass skipped as the
		// live account. It is fetched through the live path now, so whatever was
		// armed against its snapshot no longer describes anything.
		for n := range held {
			if !listed[n] {
				delete(held, n)
			}
		}

		mu.Lock()
		last = fresh
		backoff = held
		order = reorderFailedFirst(fetchOrder, failed)
		mu.Unlock()
		// Sorted by name for the result, independent of fetchOrder: the header
		// (dedupeAccounts) reads Accounts in this order directly, and without
		// sorting here a name would jump around the header block whenever it
		// failed and got promoted to the front of the next pass's fetch order.
		// account_list.go's own consumers (the Ctrl+W picker, `account list`)
		// already re-sort by name regardless, so this changes nothing for them.
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
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
// Vintage is judged per account, not per file, because entries in one file
// don't share one: a carried-forward entry's numbers can be much older than
// the write that persisted them, and each keeps its own FetchedAt rather than
// the envelope's. fresh is the same gate a live carry-forward uses (no age
// bound, just "does this entry actually have numbers and a timestamp"), so an
// entry that survives a restart is exactly one that would have survived
// without one. A cache file written before per-account timestamps existed
// decodes to the zero time and is therefore dropped whole — intended: one
// poll replaces it, and there is no vintage to speak of for numbers with no
// timestamp at all.
//
// Every seeded entry is marked Stale. This is a disk read, not a completed
// poll — saveKnownAccountsCache never persists Stale (only genuinely fetched
// entries reach disk), so without this every seed would decode as an ordinary
// fresh reading and render exactly like one just fetched, for however long it
// takes the first real pass to land.
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
		a.Stale = true
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
// failed-fetch backoff, same pause/resume/kick surface). Runs in the TUI
// client only (tui.go), against this machine's own snapshots — a
// `claude-sessions -s` server never starts one; a remote's known accounts are
// read straight off disk on demand by GET /usage instead (server.go).
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
