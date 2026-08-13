package main

import (
	"encoding/json"
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
// Reason is a short fixed tag: classifyUsageErr's classification for an actual
// HTTP answer ("rate limited", "server error", …), or set directly for a
// failure with no HTTP answer to classify ("bad snapshot", "needs refresh").
// Never raw error text.
type KnownAccountUsage struct {
	Name    string     `json:"name"`    // snapshot name, e.g. "avisoma"
	Account string     `json:"account"` // email, "" if account.json missing/unreadable
	Info    *UsageInfo `json:"info"`    // fresh, or (with Stale) carried forward; nil when Expired
	Expired bool       `json:"expired"` // a genuine 401/403 only — the actionable "re-login" state
	Stale   bool       `json:"stale,omitempty"`
	// Reason is set whenever this entry isn't a clean fetch: alongside Expired,
	// alongside either carried-forward or absent numbers, or — for a failure
	// that never reached the network, like needs-refresh — alone with neither.
	// Empty only on a clean fetch.
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
	// being restamped on every re-serve. The header now renders it: the Stale
	// marker carries the reading's age ("stale 12m"), formatAge over
	// now-FetchedAt, threaded in as accountUsageLine.fetchedAt — so restamping
	// would not merely mislead a wire consumer, it would show a permanently
	// throttled account's numbers as permanently fresh right here.
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

// snapshotAccessTokenExpiresAt reads when one snapshot's OAuth *access* token
// ages out, or the zero time when the credential does not say (msExpiry's
// "0 means unknown" contract, the same reading validateSnapshotCredential
// takes of the identical field).
//
// It deliberately re-reads the file snapshotToken just read rather than
// widening that function's return: snapshotToken is the shared parse the live
// path uses too, and its one-string contract is what several call sites and
// tests are written against. Two small local file reads per account per pass
// cost nothing next to the HTTP round trip this pair of reads exists to avoid,
// and the only way they can disagree is an `account save` landing between
// them — worth one wasted request, or one spurious "needs refresh" that the
// next pass corrects, neither of which is worth coupling the two reads to
// prevent.
func snapshotAccessTokenExpiresAt(name string) (time.Time, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return time.Time{}, err
	}
	data, err := os.ReadFile(snapshotCredentialPath(home, name))
	if err != nil {
		return time.Time{}, err
	}
	creds, err := parseOAuthCredentialBlob(data)
	if err != nil {
		return time.Time{}, err
	}
	return msExpiry(creds.ExpiresAt), nil
}

// usageExpiryClockSkew tolerates a small clock disagreement between this host
// and Anthropic's before a parked access token is treated as expired. It is
// deliberately one-directional: being a minute late to notice costs one
// request that would have failed anyway, while being a minute early would
// withhold a reading from a token that still works.
const usageExpiryClockSkew = 60 * time.Second

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

// usageNeedsRefreshReason is the classification for a parked snapshot whose
// OAuth *access* token has aged out while its refresh token is still good.
// Nothing in this codebase can fix that: claude-switch snapshots are strictly
// read-only here (see snapshotToken), so the token can never rotate in place
// the way the live credential's does, and the account stays in this state
// until a human runs `account save <name>` while logged into it.
//
// It shares usageBadSnapshotReason's shape — set on the entry directly, no
// classifyUsageErr branch, because no request was made — and is deliberately
// neither of the two tags it would otherwise fall in with:
//
//   - not usageExpiredReason: "auth expired" means re-login, and the refresh
//     token here is still perfectly valid. Only the snapshot is stale.
//   - not usageRateLimitedReason: which is what this state used to render as,
//     and the whole reason this tag exists. Verified live against the real
//     endpoint — an expired access token is answered 429 (with a Retry-After
//     of ~30 minutes), not 401 — so classifyUsageErr saw an ordinary throttle,
//     the consecutive-429 backoff armed a real wait, and an account that
//     could never heal on its own sat there cycling "rate limited" forever
//     against a fix that is one command away.
const usageNeedsRefreshReason = "needs refresh"

// knownAccountUsage is fetchKnownAccountUsage with the HTTP leg injected, so
// tests exercise every branch without a network or a real token.
//
// A failure splits three ways. A genuine 401/403 is Expired and carries NO
// numbers forward — the credential is dead, and bars beside a dead credential
// would imply it still works. Any other failure (429, 5xx, timeout, network, an
// unreadable credential file) re-serves prev's numbers marked Stale, with
// prev's ORIGINAL FetchedAt: restamping it to now would let an account that
// fails forever look permanently fresh to any consumer of the timestamp — this
// TUI's own header included, since the Stale marker now renders that age
// ("stale 12m") rather than a bare fixed label. Carrying is not age-bounded —
// old numbers beside a visible
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
	// The token parses, but has it aged out? A parked snapshot's access token
	// expires like any other, and unlike the live credential nothing ever
	// refreshes this copy, so spending a request on it is a request that cannot
	// succeed today and cannot succeed on any later pass either. Answering it
	// from the file is both cheaper and more truthful — see
	// usageNeedsRefreshReason for what the endpoint actually says instead.
	//
	// usageClockNow, not time.Now: this is a *decision* the fetcher tests drive
	// through the same clock seam they drive backoff with (fakeClock), unlike
	// the FetchedAt stamp below, which records when a fetch really happened.
	//
	// A zero expiry means the credential never said (msExpiry's contract), and
	// an unreadable one means the file changed under us between snapshotToken
	// and here — neither is evidence of expiry, so both fall through to the
	// fetch that would have happened anyway.
	if exp, expErr := snapshotAccessTokenExpiresAt(name); expErr == nil && !exp.IsZero() &&
		usageClockNow().After(exp.Add(usageExpiryClockSkew)) {
		return failed(false, usageNeedsRefreshReason, false)
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
// pre-timestamp disk write) is the only thing this rejects. carryable is the
// only caller — this is its numbers-and-info half, before carryable adds its
// own identity test on top.
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
// time, inline in the loop below (sequential, not fanned out — several
// different Anthropic accounts, each with its own budget, but all requests
// from this one process to the same endpoint at the same moment, so spreading
// them out in time avoids presenting as a burst). The live email is re-read
// per fetch, like fetchUsage does, so a relogin mid-run immediately changes
// both which snapshot is skipped and which one is reported as active. Only an
// unresolvable home directory is an error; zero snapshots is an empty list.
//
// Unlike the version of this function that predates account_cache.go, the
// closure itself holds no memory at all: every pass reads each account's own
// cache entry fresh from disk (loadAccountCache) instead of maintaining an
// in-memory `last`/`backoff` map rebuilt from a construction-time seed. That
// is what makes an account switch a non-event on this side too — a name that
// stops being skipped (its email no longer matches who's live) picks up
// whatever was persisted for it, including if that account was itself live a
// moment ago, with no special-casing needed to notice the switch happened.
// Reading N small files every 2-minute pass is negligible next to the actual
// per-account HTTP round trips this function makes; see CLAUDE.md's "Usage
// polling" section for the full reasoning behind the shared per-account file.
//
// The only memory the closure DOES keep is the fetch order — WHICH account
// goes first, since accounts are fetched one at a time rather than fanned
// out all at once. Any account this pass could not get fresh
// numbers for — a genuine failure or a synthesized backoff answer,
// `Expired || Reason != ""` either way — moves to the front for the next pass
// (reorderFailedFirst), ahead of the accounts that are fine. reconcileFetchOrder
// folds in additions and removals each pass so a renamed or newly-saved
// snapshot is never silently dropped from the rotation. The returned result is
// sorted by name regardless of fetchOrder (see below), so this reordering is
// invisible to the header and every other consumer — it only changes request
// sequence, not content. order is read and written only inside the returned
// closure, which usagePoller.run calls one tick at a time from its single
// goroutine, so mu guards nothing that is genuinely contended — kept anyway
// to match every other closure in this file that carries state this shape.
//
// save is injected rather than called as saveAccountCache directly, the same
// reason newUsageFetcher's save is: a hardcoded call would make every test
// that exercises a real fetch write to the developer's actual TMPDIR. load
// (loadAccountCache) is called directly, not injected — reads are lower risk
// than writes and every test already isolates TMPDIR the same way tests
// isolate HOME for loadAccountEmail, matching this file's existing precedent.
func newKnownAccountsFetcher(save func(name string, e accountCacheEntry)) func() (*knownAccountsResult, error) {
	var mu sync.Mutex
	var order []string
	return func() (*knownAccountsResult, error) {
		names, err := snapshotAccountNames()
		if err != nil {
			return nil, err
		}
		now := usageClockNow()
		mu.Lock()
		fetchOrder := reconcileFetchOrder(order, names)
		mu.Unlock()

		liveEmail := loadAccountEmail()
		accounts := make([]KnownAccountUsage, 0, len(fetchOrder))
		active := ""
		failed := make(map[string]bool, len(fetchOrder))
		for _, name := range fetchOrder {
			email := snapshotAccountEmail(name)
			if emailMatchesLive(email, liveEmail) {
				if active == "" {
					active = name
				}
				continue
			}
			cached, clamped := loadAccountCache(name)
			if clamped {
				save(name, cached)
			}
			var prev *KnownAccountUsage
			if cached.Info != nil {
				// Handed to knownAccountUsage regardless of whether cached.Account
				// matches email below — its own carryable(prev, email) check is
				// what actually decides whether these numbers may be carried
				// forward, and rejects a mismatch on its own.
				prev = &KnownAccountUsage{
					Name: name, Account: cached.Account, Info: cached.Info,
					Stale: cached.Stale, Verified: cached.Verified, FetchedAt: cached.FetchedAt,
				}
			}
			backoff := cached.backoff()
			if !entryIdentityMatches(cached, email) && cached.Account != "" {
				// Unlike numbers (guarded by knownAccountUsage's own carryable
				// check above), nothing gated the backoff half until an
				// independent review caught it: a claude-switch snapshot name
				// reassigned to a different account (`account save --force`)
				// would otherwise inherit the outgoing account's armed wait
				// even though the new account was never itself throttled.
				backoff = usageBackoff{}
			}
			attempted := backoff.due(now)
			// Read-before-fetch coalescing: another process (or this one, a
			// moment ago) already has a reading recent enough that a round
			// trip here would be redundant. carryable is the same identity
			// gate the ordinary carry-forward path (inside knownAccountUsage's
			// failed helper) already trusts, so nothing is served here that
			// wasn't already safe to re-serve. Reason is cleared: this is
			// being treated as equivalent to a fresh success, not a failure.
			coalesced := attempted && carryable(prev, email) && !prev.FetchedAt.After(now) && now.Sub(prev.FetchedAt) < usageCoalesceWindow
			var r *KnownAccountUsage
			switch {
			case coalesced:
				// served, not fresh: the package-level fresh() lives in this
				// same file, and shadowing it here would be a trap for the
				// next reader even though nothing in this arm calls it.
				served := *prev
				served.Stale, served.Reason = false, ""
				r = &served
				attempted = false
			case attempted:
				r, _ = fetchKnownAccountUsage(name, liveEmail, prev)
			default:
				// A backed-off account still goes through knownAccountUsage, with the
				// answer a throttled endpoint would have given substituted for the
				// round trip. That keeps one place deciding whether this name stands
				// for the live account, whether prev's numbers may be carried
				// forward, and how the entry reads.
				r, _ = knownAccountUsage(name, liveEmail, prev, func(string) (*UsageInfo, error) {
					return nil, &usageHTTPError{Status: http.StatusTooManyRequests}
				})
			}
			if r == nil {
				continue
			}
			accounts = append(accounts, *r)
			// Anything that isn't a clean success — Expired, or any Reason at all
			// (a genuine failure this pass, or a synthesized backoff answer) —
			// earns the front of the queue next time.
			if r.Expired || r.Reason != "" {
				failed[name] = true
			}
			// A synthesized entry is not an attempt, so it neither lengthens the
			// wait nor resets it — the deadline already armed simply stands.
			if attempted {
				if r.Reason == usageRateLimitedReason {
					backoff = backoff.next(now, r.RetryAt)
				} else {
					// Numbers, a dead credential, a failure of some other kind — all
					// end the streak. The state tracks consecutive throttles only.
					backoff = usageBackoff{}
				}
			}
			// Account is persisted whether or not this pass produced numbers:
			// knownAccountUsage sets it from snapshotAccountEmail on every
			// outcome, and it is what binds the backoff half beside it to an
			// identity. Written only alongside Info — as it was — a chronically
			// failing account persisted its armed wait under Account "", which
			// entryIdentityMatches (above) can never match and, worse, can never
			// even *reject*: the `cached.Account != ""` guard meant a snapshot
			// name reassigned to a different account inherited the outgoing
			// account's wait unchallenged. That is the same hole
			// entryIdentityMatches was introduced to close, reached through the
			// one entry shape it could not see. FetchedAt/Info/Verified stay
			// inside the conditional — they describe numbers, and there are none.
			//
			// This closes the hole only from this side. usage.go's own persist
			// (the live account's poller) has the identical gap on a failed pass
			// with nothing carryable — untouched here — and both hubs write the
			// SAME per-account file across a switch (see account_cache.go), so a
			// live-side failure can still leave Account "" for the next reader.
			// Not fixed in this change; a real gap, not a regression from it.
			e := accountCacheEntry{
				Account:            r.Account,
				BackoffStreak:      backoff.streak,
				BackoffNextAttempt: backoff.nextAttempt,
			}
			if r.Info != nil {
				e.FetchedAt, e.Info, e.Verified = r.FetchedAt, r.Info, r.Verified
			}
			if !coalesced {
				save(name, e)
			}
		}

		mu.Lock()
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

// KnownAccountsHub polls every account this host holds a claude-switch
// credential snapshot for, alongside UsageHub's poll of the live account (see
// usagePoller for the shared mechanism: same 2-minute cadence, same
// failed-fetch backoff, same pause/resume/kick surface). Runs in the TUI
// client only (tui.go), against this machine's own snapshots — a
// `claude-sessions -s` server never starts one; a remote's known accounts are
// read straight off disk on demand by GET /usage instead (server.go).
type KnownAccountsHub = usagePoller[knownAccountsResult]

// seedKnownAccounts builds the poller's initial display straight from each
// non-live account's own accountCacheEntry, without waiting on the fetcher's
// own first pass (which reaches the identical entries a moment later anyway,
// via loadAccountCache inside newKnownAccountsFetcher).
func seedKnownAccounts() *knownAccountsResult {
	names, err := snapshotAccountNames()
	if err != nil || len(names) == 0 {
		return nil
	}
	liveEmail := loadAccountEmail()
	accounts := make([]KnownAccountUsage, 0, len(names))
	for _, name := range names {
		if emailMatchesLive(snapshotAccountEmail(name), liveEmail) {
			continue
		}
		e, _ := loadAccountCache(name)
		if e.Info == nil || e.FetchedAt.IsZero() {
			// The zero-timestamp guard matches fresh()'s own gate elsewhere in
			// this file: an unstamped entry (a pre-timestamp disk write, in
			// practice never produced by a current writer) must not seed the
			// header with numbers the fetcher's own carryable()/fresh() would
			// then refuse to carry forward — the seed and the carry rule have
			// to agree on what counts as usable.
			continue
		}
		accounts = append(accounts, KnownAccountUsage{
			Name: name, Account: e.Account, Info: e.Info, Stale: e.Stale, Verified: e.Verified, FetchedAt: e.FetchedAt,
		})
	}
	if len(accounts) == 0 {
		return nil
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return &knownAccountsResult{Accounts: accounts}
}

// NewKnownAccountsHub starts the poller and returns immediately, seeded from
// each account's own recent disk cache (seedKnownAccounts) so a restart shows
// (stale) bars immediately rather than waiting on the fetcher's own first
// pass.
func NewKnownAccountsHub() *KnownAccountsHub {
	return newUsagePoller(seedKnownAccounts(), newKnownAccountsFetcher(saveAccountCache), func(*knownAccountsResult) {})
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
