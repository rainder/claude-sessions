package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// accountCacheEntry is one account's on-disk cache: numbers and backoff state
// together, so whichever role an account is currently playing — live or
// known — reads and writes the exact same record. Before this type existed,
// the live and known-account hubs each owned separate files
// (usageCachePath/usageBackoffCachePath vs
// knownAccountsCachePath/knownAccountsBackoffCachePath), so switching accounts
// lost continuity in both directions: the account you switched TO started its
// numbers over from nothing on the live side, and the account you switched
// AWAY FROM lost its backoff streak the moment it re-appeared on the known
// side. See CLAUDE.md's "Usage polling" section.
type accountCacheEntry struct {
	// Account is the email this entry's numbers/backoff belong to — kept as
	// plain data, never the storage key (see accountCachePath). Every re-serve
	// is still gated on this matching the live/claimed email at use time
	// (liveCarryable, carryable), exactly as before this type existed; this is
	// a storage-layer change only, not a change to identity semantics.
	Account   string     `json:"account"`
	FetchedAt time.Time  `json:"fetched_at,omitzero"`
	Info      *UsageInfo `json:"info,omitempty"`
	Stale     bool       `json:"stale,omitempty"`
	// Verified mirrors KnownAccountUsage.Verified: only a verified reading may
	// be carried forward past a wrong-identity failure. See that type's doc
	// comment for the full reasoning.
	Verified bool `json:"verified,omitempty"`
	// BackoffStreak/BackoffNextAttempt are a usageBackoff, flattened for the
	// JSON encoding the same way cachedUsageBackoff/cachedKnownAccountsBackoffEntry
	// used to.
	BackoffStreak      int       `json:"backoff_streak,omitempty"`
	BackoffNextAttempt time.Time `json:"backoff_next_attempt,omitzero"`
}

// backoff extracts this entry's usageBackoff.
func (e accountCacheEntry) backoff() usageBackoff {
	return usageBackoff{streak: e.BackoffStreak, nextAttempt: e.BackoffNextAttempt}
}

// empty reports whether there is nothing in this entry worth keeping on disk
// — no numbers and no active wait. An Account with nothing else is still not
// worth a file.
func (e accountCacheEntry) empty() bool {
	return e.Info == nil && e.BackoffStreak <= 0
}

// accountCachePath is the cache file for one account, named by its
// claude-switch snapshot name — never by email, since an account whose
// .{name}.account.json is unreadable (email "") must still get a stable slot
// to persist real backoff state into, or it gets fetched (and likely
// re-throttled) on every single pass. name is always a value
// snapshotAccountNames() produced, which is already filesystem-safe — it came
// FROM a filename on this same filesystem.
func accountCachePath(name string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("claude-sessions-account-%d-%s.json", os.Getuid(), name))
}

// saveAccountCache persists one account's entry, or removes its file when
// there is nothing left worth keeping (e.empty()) — "nothing cached, nothing
// waiting" needs no file on disk, the same rule every backoff cache in this
// codebase already follows. Writes via writeFileAtomic (account.go) — a
// temp-file-then-rename, not a truncating os.WriteFile — because this file
// can legitimately have two writers now (the live hub and the known-accounts
// hub both touch the SAME account's file across an account switch, where
// before they wrote entirely separate files by role) and a reader must never
// be able to observe a half-written entry. Best-effort beyond that, and for
// a resolvable snapshot name this is a real, currently-accepted gap, not
// just a warm-start cost: the disk path (newUsageFetcher/
// newKnownAccountsFetcher) keeps no in-memory copy of what it last wrote —
// every pass reloads from disk — so a save() that silently fails (e.g. a
// read-only TMPDIR) means that pass's backoff streak is never remembered by
// the next one, and the account gets fetched (and likely re-throttled)
// every pass instead of backing off. The fbXXX in-memory fallback in
// newUsageFetcher does not cover this: it only engages when the name fails
// to resolve at all, not when it resolves but the write fails. Unlike a
// resolvable name, a live account with NO matching snapshot is unaffected —
// see TestUsageFetcherBacksOffEvenWithNoMatchingSnapshot.
func saveAccountCache(name string, e accountCacheEntry) {
	if e.empty() {
		_ = os.Remove(accountCachePath(name))
		return
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = writeFileAtomic(accountCachePath(name), data, 0600)
}

// loadAccountCache returns one account's persisted entry, or the zero value
// if there is none, it is unreadable, or it carries nothing usable. clamped
// reports whether the returned BackoffNextAttempt was reduced from what was
// actually on disk — see below; the caller (which already holds an injected
// save function) is responsible for writing the correction back through that
// same seam, so this function itself never touches disk on a read path. That
// split exists specifically so tests can drive loadAccountCache freely
// without also needing to isolate TMPDIR the way a write would require.
//
// A non-nil Info is always marked Stale on load: this is a disk read, not a
// completed poll, so nothing here has confirmed the numbers as current — the
// same rule loadUsageCache/loadKnownAccountsCache followed before this type
// replaced them.
//
// The backoff half is NOT reset just because its deadline has already
// elapsed: due() (usageBackoff.due) already reports the account fetchable
// once nextAttempt has passed, with no help needed from this loader, and this
// function is now called on every single pass rather than once at process
// startup (the whole point of newUsageFetcher/newKnownAccountsFetcher no
// longer keeping in-memory state — see their doc comments). A streak of 1 has
// nextAttempt == the instant it was recorded (wait is zero by design — see
// usageBackoffUntil), which means it reads as "elapsed" on the very next
// load, microseconds later, as part of completely ordinary back-to-back
// polling — an earlier version of this function dropped the streak in
// exactly that case, which silently prevented a streak from EVER reaching 2
// and arming a real wait, because every streak-1 entry looked elapsed before
// its second throttle could land. A deadline that HAS NOT yet elapsed is
// still clamped to now+usageBackoffCeiling — without that, a corrupt or
// bogus far-future timestamp would wedge an account out of rotation for
// longer than a live-armed wait ever could.
func loadAccountCache(name string) (e accountCacheEntry, clamped bool) {
	data, err := os.ReadFile(accountCachePath(name))
	if err != nil {
		return accountCacheEntry{}, false
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return accountCacheEntry{}, false
	}
	if e.Info != nil {
		e.Stale = true
	}
	if e.BackoffStreak <= 0 || e.BackoffNextAttempt.IsZero() {
		e.BackoffStreak, e.BackoffNextAttempt = 0, time.Time{}
	} else if max := time.Now().Add(usageBackoffCeiling); e.BackoffNextAttempt.After(max) {
		e.BackoffNextAttempt = max
		clamped = true
	}
	return e, clamped
}

// entryIdentityMatches reports whether a loaded entry's claimed Account may
// be trusted for claimedEmail — the identity gate that liveCarryable/carryable
// apply to an entry's numbers, extended here to cover its backoff half too.
// A loaded entry whose Account disagrees with who is actually live (a stale
// file, or a claude-switch snapshot name reused for a different account via
// `account save --force`) must not have EITHER half trusted: the numbers
// gate already existed (liveCarryable/carryable), but the backoff half was
// extracted and used unconditionally until an independent review caught it —
// a reused name would otherwise inherit the outgoing account's armed wait
// even though it was never actually throttled itself. An empty Account (never
// verified) or an empty claimedEmail (nothing to compare against) never
// matches — identity can't be confirmed either way.
func entryIdentityMatches(e accountCacheEntry, claimedEmail string) bool {
	return e.Account != "" && claimedEmail != "" && strings.EqualFold(e.Account, claimedEmail)
}

// resolveActiveSnapshotName finds which claude-switch snapshot name — if any
// — belongs to liveEmail, i.e. which stored snapshot the current login
// corresponds to. It is the one place the live account's cache resolves
// "which per-account slot am I" (newUsageFetcher) and mirrors the identical
// inline loop allKnownAccounts' callers already use to resolve the same fact
// for known-account skipping and /usage's activeSnapshotName — kept separate
// from those rather than factored further, since each has its own loop body
// to run per name (skip-and-continue vs match-and-return); this is only the
// "first match" shape. Returns "" when liveEmail is unknown or matches no
// snapshot; an error only for an unresolvable home directory.
func resolveActiveSnapshotName(liveEmail string) (string, error) {
	names, err := snapshotAccountNames()
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if emailMatchesLive(snapshotAccountEmail(name), liveEmail) {
			return name, nil
		}
	}
	return "", nil
}
