package main

import (
	"crypto/sha256"
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

// usageBucket is one rate-limit window from the OAuth usage endpoint.
type usageBucket struct {
	Pct      float64
	ResetsAt time.Time
}

// creditsInfo is the extra-usage (pay-as-you-go credits) state from the
// usage endpoint. Amounts are in minor currency units (e.g. cents when
// DecimalPlaces is 2).
type creditsInfo struct {
	Enabled       bool
	Used          float64
	Limit         float64
	Currency      string
	DecimalPlaces int
}

// Pct is credits utilization 0–100 (the endpoint's own utilization field is
// often null, so it's derived from used/limit).
func (c creditsInfo) Pct() float64 {
	if c.Limit <= 0 {
		return 0
	}
	return c.Used / c.Limit * 100
}

// UsageInfo is the parsed account rate-limit snapshot shown in the header.
type UsageInfo struct {
	FiveHour usageBucket
	SevenDay usageBucket
	// WeeklyScoped is the model-scoped weekly limit (the "limits" array's
	// weekly_scoped entry). Zero value with an empty WeeklyScopedLabel means
	// the account has no scoped limit and the header hides that bar.
	WeeklyScoped      usageBucket
	WeeklyScopedLabel string
	Credits           creditsInfo
	// VerifiedAccount is the login email the OAuth profile endpoint attributed
	// the very token that fetched these numbers to — the identity of the
	// numbers themselves, rather than of whatever file the caller happened to
	// read a token out of. "" when the profile probe failed or was not made;
	// identity then falls back to the file read it always used.
	//
	// It exists because pairing a token from one store (the Keychain, a
	// snapshot file) with an email from another (~/.claude.json,
	// .<name>.account.json) is a read-order race at best and a silent
	// misattribution at worst: a still-running session of the outgoing account
	// can rewrite the live credential moments after a switch, leaving the two
	// stores describing different accounts with nothing to notice it.
	//
	// Not serialized. Identity is verified where the token lives, so a remote
	// host's answer already carries its own verified AccountUsage.Account /
	// KnownAccountUsage.Account; shipping this too would give one fact two
	// writers on the wire. It is likewise absent from the disk caches, which is
	// harmless — a seeded reading is re-verified by the first live fetch.
	VerifiedAccount string `json:"-"`
}

// AccountUsage pairs a rate-limit snapshot with the account it belongs to, so a
// remote host's limits stay attributable when it runs a different Anthropic
// account than the client. Account is the login email ("" when the identity
// couldn't be read); Info is the snapshot (nil before the first fetch lands).
type AccountUsage struct {
	Account string     `json:"account"` // email, "" when unknown
	Info    *UsageInfo `json:"info"`
	// Stale marks numbers carried forward from an earlier poll because the
	// latest one was skipped by the backoff, mirroring KnownAccountUsage.Stale:
	// the bars still render (old numbers beat no numbers) beside a dim marker,
	// and localFreshAccountEmails treats the account as one this machine cannot
	// currently vouch for.
	Stale bool `json:"stale,omitempty"`
	// FetchedAt is when Info was actually fetched, not when this struct was
	// built — a carried-forward reading keeps the original timestamp rather
	// than being restamped on every re-serve. The header now renders it: the
	// Stale marker carries the reading's age ("stale 12m"), formatAge over
	// now-FetchedAt, threaded in as accountUsageLine.fetchedAt — so restamping
	// would not merely mislead a wire consumer, it would show a permanently
	// throttled account's numbers as permanently fresh right here. Only
	// fetchUsage sets it; the server's on-demand /usage handler has no memory
	// to carry anything forward, so it never produces a Stale reading either.
	//
	// omitzero, not omitempty: encoding/json never considers a struct empty, so
	// omitempty on a time.Time does nothing and every snapshot with no numbers
	// would ship a "0001-01-01T00:00:00Z".
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
}

// parseUsage decodes the /api/oauth/usage response body. The overall
// five_hour and seven_day buckets are kept; per-model buckets are ignored by
// design. The model-scoped weekly limit is pulled from the "limits" array's
// weekly_scoped entry (label carried from its scope.model.display_name); its
// absence is not an error — the header just hides that bar.
func parseUsage(body []byte) (*UsageInfo, error) {
	type bucket struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	}
	var raw struct {
		FiveHour   *bucket `json:"five_hour"`
		SevenDay   *bucket `json:"seven_day"`
		ExtraUsage *struct {
			IsEnabled     bool    `json:"is_enabled"`
			MonthlyLimit  float64 `json:"monthly_limit"`
			UsedCredits   float64 `json:"used_credits"`
			Currency      string  `json:"currency"`
			DecimalPlaces int     `json:"decimal_places"`
		} `json:"extra_usage"`
		Limits []struct {
			Kind     string    `json:"kind"`
			Group    string    `json:"group"`
			Percent  float64   `json:"percent"`
			ResetsAt time.Time `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.FiveHour == nil || raw.SevenDay == nil {
		return nil, fmt.Errorf("usage response missing five_hour/seven_day")
	}
	u := &UsageInfo{
		FiveHour: usageBucket{Pct: raw.FiveHour.Utilization, ResetsAt: raw.FiveHour.ResetsAt},
		SevenDay: usageBucket{Pct: raw.SevenDay.Utilization, ResetsAt: raw.SevenDay.ResetsAt},
	}
	// First weekly_scoped entry wins (fallback: any weekly limit with a named
	// model scope). is_active is intentionally ignored — the live response's
	// scoped entry is often is_active:false, and filtering it would drop the
	// bar.
	for _, l := range raw.Limits {
		scoped := l.Kind == "weekly_scoped" ||
			(l.Group == "weekly" && l.Scope != nil && l.Scope.Model != nil)
		if !scoped || l.Scope == nil || l.Scope.Model == nil || l.Scope.Model.DisplayName == "" {
			continue
		}
		u.WeeklyScoped = usageBucket{Pct: l.Percent, ResetsAt: l.ResetsAt}
		u.WeeklyScopedLabel = l.Scope.Model.DisplayName
		break
	}
	if e := raw.ExtraUsage; e != nil {
		u.Credits = creditsInfo{
			Enabled:       e.IsEnabled,
			Used:          e.UsedCredits,
			Limit:         e.MonthlyLimit,
			Currency:      e.Currency,
			DecimalPlaces: e.DecimalPlaces,
		}
	}
	return u, nil
}

// loadAccountEmail reads the logged-in Anthropic account's email from
// oauthAccount.emailAddress in $HOME/.claude.json (Claude Code's top-level
// config — note this is NOT ~/.claude/.claude.json). Returns "" on any error;
// the header just renders the bars without an account label. Read-only, like
// the token — Claude Code owns this file.
func loadAccountEmail() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
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

// usageURL is the endpoint Claude Code's /usage command reads.
const usageURL = "https://api.anthropic.com/api/oauth/usage"

// profileURL answers who a token belongs to: {"account":{"email":…},…}. Verified
// directly against the live endpoint with the same headers fetchUsageInfo sends
// (the usage response itself carries no identity at all, which is why this is a
// second request rather than one more field to parse).
const profileURL = "https://api.anthropic.com/api/oauth/profile"

// loadOAuthToken reads Claude Code's OAuth access token: from the login
// Keychain on macOS (exec'd `security`, no cgo), from
// ~/.claude/.credentials.json elsewhere. Read-only — Claude Code owns the
// token's refresh/rotation, which is why this is re-read on every fetch.
func loadOAuthToken() (string, error) {
	var data []byte
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password",
			"-s", "Claude Code-credentials", "-w").Output()
		if err != nil {
			return "", fmt.Errorf("keychain read: %w", err)
		}
		data = out
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		data, err = os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
		if err != nil {
			return "", err
		}
	}
	return parseOAuthCredentials(data)
}

// parseOAuthCredentials pulls the OAuth access token out of a Claude Code
// credentials blob — the JSON the macOS Keychain item holds, which is also
// byte-for-byte what ~/.claude/.credentials.json stores and what
// claude-switch copies into its per-account snapshot files. Shared by the live
// path (loadOAuthToken) and the snapshot path (snapshotToken) so both parse
// identically; an empty token is an error, not an empty string.
func parseOAuthCredentials(data []byte) (string, error) {
	creds, err := parseOAuthCredentialBlob(data)
	if err != nil {
		return "", err
	}
	if creds.AccessToken == "" {
		return "", fmt.Errorf("no access token in credentials")
	}
	return creds.AccessToken, nil
}

// oauthCredentials is the part of a Claude Code credential blob this tool reads.
// Only the access token is needed to spend one; the refresh token and the two
// expiries are what tell a *snapshot* apart from a usable login, which is what
// `account switch` validates before installing one (see
// validateSnapshotCredential).
//
// Both expiries are milliseconds since the epoch, matching what Claude Code
// writes; 0 means the field was absent, which is treated as "unknown", never as
// "expired in 1970".
type oauthCredentials struct {
	AccessToken           string `json:"accessToken"`
	RefreshToken          string `json:"refreshToken"`
	ExpiresAt             int64  `json:"expiresAt"`
	RefreshTokenExpiresAt int64  `json:"refreshTokenExpiresAt"`
}

// expiry converts one of the millisecond fields to a time, or the zero time when
// the field was absent — so callers test IsZero() for "unknown" rather than
// having to know the wire units.
func msExpiry(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// parseOAuthCredentialBlob decodes a credential blob — the JSON the macOS
// Keychain item holds, byte-for-byte what ~/.claude/.credentials.json stores and
// what claude-switch copies into its per-account snapshots.
func parseOAuthCredentialBlob(data []byte) (oauthCredentials, error) {
	var creds struct {
		ClaudeAiOauth oauthCredentials `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return oauthCredentials{}, fmt.Errorf("parse credentials: %w", err)
	}
	return creds.ClaudeAiOauth, nil
}

// fetchUsage hits the usage endpoint with the current token and attributes the
// result to the account that token actually belongs to (UsageInfo.VerifiedAccount,
// from the profile endpoint), falling back to the logged-in email on disk when
// the profile probe could not answer.
//
// The fallback email is captured immediately beside the token read, before the
// round trip, and never re-read afterwards. Pairing a token read at T0 with an
// email read at T1 is a race in its own right — a relogin or a switch in that
// window labels one account's numbers with another's — and the whole reason this
// function now asks the endpoint instead of inferring from two files.
//
// A verified email that disagrees with the file wins, and deliberately so: it is
// the ground truth for whose budget these numbers came out of, and the header
// showing an unexpected address is exactly how a credential clobbered by a
// still-running session of the outgoing account becomes visible at all.
//
// This is the only place AccountUsage.FetchedAt is stamped: it dates the numbers
// themselves, so everything that re-serves them (newUsageFetcher) carries the
// original timestamp forward rather than restamping it.
func fetchUsage() (*AccountUsage, error) {
	tok, err := loadOAuthToken()
	if err != nil {
		return nil, err
	}
	// Read adjacent to the token, not after the network call — see above.
	fileEmail := loadAccountEmail()
	info, err := usageInfoFetch(tok)
	if err != nil {
		return nil, err
	}
	return &AccountUsage{Account: usageAccountLabel(fileEmail, info), Info: info, FetchedAt: time.Now()}, nil
}

// fetchVerifiedUsageInfo is the production value of usageInfoFetch: one usage
// fetch plus, on success only, one profile fetch with the same token to record
// whose numbers these are.
//
// Only a successful usage fetch pays for the second round trip. An account being
// throttled or a credential that is dead already has its answer, and asking a
// second endpoint about it would double the cost of exactly the failure the
// consecutive-429 backoff exists to stop paying for.
//
// A profile failure is never an error here. Identity verification is an upgrade
// over reading an email from a file, not a precondition for reporting numbers:
// every caller falls back to the file read it used before, so an offline or
// throttled profile endpoint costs nothing but the upgrade.
//
// **The two legs are sequential.** The usage request allows 5s
// (fetchUsageInfo) and the profile request 2s (profileFetchTimeout), so this
// function's worst case is ~7s. This budget belongs to the client-side
// fetchers only (newUsageFetcher, newKnownAccountsFetcher) — the server's
// GET /usage handler no longer calls this function at all, so there is no
// server-side timeout wrapper in this chain to check it against.
//
// The verified email is also cached per token (profileEmails), so the second
// round trip is paid once per credential rather than on every poll tick.
func fetchVerifiedUsageInfo(token string) (*UsageInfo, error) {
	return verifiedUsageInfo(token, fetchUsageInfo, profileEmails.emailFor)
}

// verifiedUsageInfo is fetchVerifiedUsageInfo with both legs injected, so the
// composition — which call happens first, which failures are fatal — is
// exercisable without a network on either endpoint.
func verifiedUsageInfo(token string,
	usage func(string) (*UsageInfo, error),
	profile func(string) (string, error)) (*UsageInfo, error) {
	info, err := usage(token)
	if err != nil {
		return nil, err
	}
	if email, perr := profile(token); perr == nil {
		info.VerifiedAccount = email
	}
	return info, nil
}

// profileEmailCacheMax bounds the token→email map. The key space is "credentials
// this process has spent" — the live one plus one per snapshot — so a handful;
// the bound exists so a long-running client that has seen many rotated tokens
// cannot grow it without limit, not because collisions are expected.
const profileEmailCacheMax = 32

// profileEmails caches which account each access token belongs to.
//
// Verification would otherwise double this process's request volume forever: the
// answer is a property of the token, and the token only changes when Claude Code
// rotates or replaces it — at which point the key changes with it and the next
// poll re-probes naturally. Nothing here needs a TTL for that reason; a stale
// entry is impossible, because a stale token is a different key.
//
// Keyed by sha256 of the token rather than the token itself so no credential
// material sits in a long-lived map (and none can be printed by an accidental
// dump of it). Only successes are cached: a failed probe must stay retryable,
// and it costs nothing to leave it out.
//
// The probe goes through the profileEmailFetch package var rather than being
// bound at construction, so the one seam tests swap still covers this path.
var profileEmails = &profileEmailCache{}

type profileEmailCache struct {
	mu    sync.Mutex
	byKey map[[32]byte]string
}

// reset drops every cached answer. Tests call it when they swap
// profileEmailFetch, since the cache outlives any single test and would
// otherwise serve one test's stubbed identity to the next.
func (c *profileEmailCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey = nil
}

// emailFor returns token's verified account email, probing at most once per
// distinct token.
func (c *profileEmailCache) emailFor(token string) (string, error) {
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	if email, ok := c.byKey[key]; ok {
		c.mu.Unlock()
		return email, nil
	}
	c.mu.Unlock()

	// Deliberately not single-flighted: unlike usageCache this guards a cheap,
	// idempotent read, and two concurrent first probes of the same token cost one
	// extra request once, where holding the mutex across the round trip would
	// serialize every account's probe behind whichever one is slowest.
	email, err := profileEmailFetch(token)
	if err != nil || email == "" {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = make(map[[32]byte]string, profileEmailCacheMax)
	}
	if len(c.byKey) >= profileEmailCacheMax {
		// No recency to preserve — every entry is equally valid until its token
		// stops being used — so the cheapest eviction is the right one: drop one
		// arbitrary entry, whose only cost is one extra probe if it comes back.
		for k := range c.byKey {
			delete(c.byKey, k)
			break
		}
	}
	c.byKey[key] = email
	return email, nil
}

// usageAccountLabel is the one rule for attributing a fetched reading to an
// account: the identity the token itself proved wins, and the email read off
// disk is the fallback for when nothing proved anything.
//
// A disagreement is not an error and is never smoothed over. It means the
// credential store and the identity cache have drifted apart — the signature of
// a switch clobbered by a still-running session of the outgoing account — and
// labelling the numbers with the account that actually produced them is what
// makes that visible in the header instead of silently wrong.
func usageAccountLabel(fileEmail string, info *UsageInfo) string {
	if info != nil && info.VerifiedAccount != "" {
		return info.VerifiedAccount
	}
	return fileEmail
}

// verifiedIdentityMismatch reports that a fetched reading provably belongs to an
// account other than the one claimed for it. Shared by every caller that fetches
// a *snapshot* account's numbers — the client poller and the server's /usage
// handler — so both classify the same file the same way.
//
// Both sides must be known for a disagreement to exist: an unverified reading
// proves nothing, and a claim of "" claims nothing.
func verifiedIdentityMismatch(claimed string, info *UsageInfo) bool {
	return claimed != "" && info != nil && info.VerifiedAccount != "" &&
		!strings.EqualFold(info.VerifiedAccount, claimed)
}

// profileEmailFetch is fetchProfileEmail behind a package var, the same seam
// usageInfoFetch is and for the same reason: a test must never spend the
// developer's own token on a real request. TestMain defaults it to a panic.
var profileEmailFetch = fetchProfileEmail

// profileFetchTimeout bounds the identity probe, and is deliberately much
// tighter than fetchUsageInfo's 5s. The probe runs *after* a successful usage
// fetch, so its budget is whatever is left over: see fetchVerifiedUsageInfo for
// the 5+2 arithmetic. Losing the identity upgrade to a slow probe costs a
// fallback to the file read; losing the numbers to one costs a cached failure.
const profileFetchTimeout = 2 * time.Second

// fetchProfileEmail asks the OAuth profile endpoint which account a token
// belongs to. Same request discipline as fetchUsageInfo — same beta header, same
// 1MB cap, same typed usageHTTPError for a non-200 — so a caller can classify a
// failure here exactly as it classifies one there, but on the tighter timeout
// above.
//
// An answer with no email is an error rather than an empty string: callers treat
// "" as "identity unknown, fall back", and a 200 that carries nothing usable is
// indistinguishable from that, so collapsing them keeps one meaning per value.
func fetchProfileEmail(token string) (string, error) {
	req, err := http.NewRequest("GET", profileURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	client := &http.Client{Timeout: profileFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := &usageHTTPError{Status: resp.StatusCode}
		if resp.StatusCode == http.StatusTooManyRequests {
			e.RetryAt = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return "", e
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var raw struct {
		Account struct {
			Email string `json:"email"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", err
	}
	if raw.Account.Email == "" {
		return "", fmt.Errorf("profile response carries no account email")
	}
	return raw.Account.Email, nil
}

// fetchUsageInfo is the HTTP leg alone: one usage fetch for an arbitrary OAuth
// token. Split out of fetchUsage so the known-accounts poller can fetch a
// snapshot account's limits with that snapshot's own token (see
// known_accounts.go) without duplicating the request/parse plumbing, and so
// tests can substitute it. 5s timeout, 1MB response cap, non-200 is an error.
func fetchUsageInfo(token string) (*UsageInfo, error) {
	req, err := http.NewRequest("GET", usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := &usageHTTPError{Status: resp.StatusCode}
		if resp.StatusCode == http.StatusTooManyRequests {
			e.RetryAt = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return nil, e
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseUsage(body)
}

// usageHTTPError is a non-200 answer from the usage endpoint, carrying the
// status so callers can tell a genuinely dead credential (401/403) apart from a
// throttle or an outage. It exists because that distinction is the difference
// between "run claude-switch and log in again" and "wait, this heals itself",
// and string-matching the message to recover it would be one refactor away from
// silently reclassifying every failure.
// RetryAt carries a 429's Retry-After, resolved to an absolute instant. It is
// best-effort sizing information for the backoff schedules below and never a
// correctness requirement: a missing, malformed or non-positive header leaves
// it zero, which every reader treats as "no opinion, use the schedule".
type usageHTTPError struct {
	Status  int
	RetryAt time.Time
}

func (e *usageHTTPError) Error() string {
	return fmt.Sprintf("usage endpoint: HTTP %d", e.Status)
}

// parseRetryAfter resolves a Retry-After header value against now. RFC 9110
// allows either form — delta-seconds or an HTTP-date — and the endpoint is free
// to pick either, so both are accepted. Anything else (empty, garbage, or a
// wait that has already elapsed by now — a non-positive delta, a date not in
// the future) is the zero time: this is advisory, so an unparseable value must
// fall back to the caller's own schedule rather than fail anything.
//
// The two forms are normalized identically on purpose. Both callers happen to
// neutralize a past deadline downstream (usageBackoffUntil only lets a
// Retry-After *lengthen* its own wait), but leaving one form returning a stale
// instant and the other the zero time means the same conceptual case has two
// shapes here, and the next reader of this value has to know which.
func parseRetryAfter(v string, now time.Time) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return time.Time{}
		}
		return now.Add(time.Duration(secs) * time.Second)
	}
	// http.ParseTime covers all three date formats HTTP allows (RFC 1123,
	// RFC 850, ANSI C asctime), which time.Parse with a single layout does not.
	if t, err := http.ParseTime(v); err == nil {
		if !t.After(now) {
			return time.Time{}
		}
		return t
	}
	return time.Time{}
}

// usageExpiredReason is the classification for the one failure a human has to
// act on. It doubles as the placeholder the header renders (usageExpiredText),
// which is why it reads as a state rather than as an error.
const usageExpiredReason = "auth expired"

// usageWrongIdentityReason is the classification for a credential snapshot whose
// token the profile endpoint attributes to a different account than the snapshot's
// own .<name>.account.json claims. Like usageBadSnapshotReason it describes the
// file rather than the endpoint, and like it the fix is `account save <name>`
// while logged into that account — but it is its own tag because the file is
// perfectly readable, and saying "bad snapshot" would send someone looking for
// corruption that isn't there.
// A mismatch is deliberately NOT carried as an error and has no entry in
// classifyUsageErr: it is detected after a fetch that succeeded, by the two call
// sites that compare the claimed email against the verified one
// (verifiedIdentityMismatch), and each sets this tag on the entry directly.
// Wrapping it in an error would mean inventing a failure for a request that
// worked.
const usageWrongIdentityReason = "wrong identity"

// usageRateLimitedReason is the classification for a 429. It is a named
// constant because two schedulers (the client's known-accounts fetcher and the
// server's usage cache) now recognise this specific tag to decide whether to
// back an account off, and a literal drifting in one of the three places would
// silently disable that.
const usageRateLimitedReason = "rate limited"

// Backoff schedule for an account the endpoint keeps throttling. One Anthropic
// account is commonly held as a credential snapshot on more than one host, and
// each host fetches it on its own usageRefreshInterval tick, so a chronically
// 429ing account otherwise costs a failed round trip per host per 2 minutes for
// the life of every process — against the very endpoint that is already
// throttling. The first 429 buys no wait from the *schedule* (throttles usually
// heal within one tick); only a *consecutive* second one starts thinning the
// retries. "Free" is no longer literal, though: usageBackoffSafetyMargin puts a
// hard 1-minute floor under every deadline this schedule computes, so even the
// streak-1 zero wait costs a minute before due() lets the next real fetch
// through.
const (
	// usageBackoffSecond is the wait after two consecutive 429s.
	usageBackoffSecond = 4 * time.Minute
	// usageBackoffMax is the wait after three or more — the schedule caps here
	// rather than growing, so an account that heals is picked up again within
	// one cap window instead of an ever-lengthening one.
	usageBackoffMax = 8 * time.Minute
	// usageBackoffCeiling bounds a Retry-After the endpoint asks for. A bogus or
	// absurd header value must not be able to wedge an account out of the
	// rotation for the life of the process. Raised from 15 minutes: a live
	// 429 was observed asking for ~31.6 minutes (Retry-After: 1895), which the
	// old ceiling silently truncated — this bound now has to cover both what a
	// real Retry-After can legitimately ask for and what a persisted deadline
	// (see account_cache.go's loadAccountCache) is clamped to on load, so it
	// needs the same headroom in both places.
	usageBackoffCeiling = time.Hour
	// usageBackoffSafetyMargin is a hard floor added on top of nextAttempt:
	// due() never allows a real fetch earlier than nextAttempt+this, even for
	// a streak-1 "free" wait (nextAttempt == the recording instant, wait=0
	// above). Applied unconditionally, not just once a real multi-minute wait
	// is armed — insurance against two processes racing to reload the same
	// disk entry (see account_cache.go: writes are best-effort, no lock), and
	// a deliberate, requested tightening of the "first 429 costs nothing"
	// rule: the very next attempt after ANY throttle now waits at least this
	// long, not zero.
	usageBackoffSafetyMargin = time.Minute
)

// usageBackoffUntil returns when an account whose last streak consecutive
// fetches were rate limited may be tried again. streak <= 1 returns now — this
// function itself imposes no wait at all for a first 429.
//
// That is a statement about this function, not about the system: due() does not
// compare against what this returns, it compares against this plus
// usageBackoffSafetyMargin (1 minute, applied unconditionally — see that
// constant). So "returns now" means "the schedule adds nothing of its own", not
// "the next ordinary tick fetches normally": the very next attempt after ANY
// throttle, including the streak-1 one this arm computes, still waits out the
// margin first. Keep the two readings separate — the arithmetic here stays the
// pure schedule, and the floor stays in one place rather than being smeared
// across every branch below.
//
// retryAt is the endpoint's own Retry-After (zero when it gave none, see
// parseRetryAfter): it can only *extend* the computed deadline, never shorten
// it — the schedule exists precisely because the endpoint's own opinion is
// often absent or optimistic — and it is capped at usageBackoffCeiling.
func usageBackoffUntil(now time.Time, streak int, retryAt time.Time) time.Time {
	var wait time.Duration
	switch {
	case streak <= 1:
		wait = 0
	case streak == 2:
		wait = usageBackoffSecond
	default:
		wait = usageBackoffMax
	}
	until := now.Add(wait)
	if retryAt.After(until) {
		until = retryAt
	}
	if ceiling := now.Add(usageBackoffCeiling); until.After(ceiling) {
		until = ceiling
	}
	return until
}

// usageClockNow is newUsageFetcher's and newKnownAccountsFetcher's shared
// clock — a package var, not a parameter, so tests can advance it without
// changing either function's signature (the same reason usageInfoFetch and
// keychainRead are package vars: TestMain never needs to know it exists).
// Production code never touches it; it is always time.Now.
var usageClockNow = time.Now

// usageBackoff is one account's consecutive-429 state. nextAttempt is always
// set (it equals the recording instant while streak is 1, i.e. "due now"), so
// "is this account backed off" is one comparison and nothing has to special-case
// a zero time.
type usageBackoff struct {
	streak      int
	nextAttempt time.Time
}

// due reports whether an account may be fetched again now. nextAttempt+
// usageBackoffSafetyMargin, not nextAttempt alone — see that constant's doc
// comment for why the margin applies even to a streak-1 "free" wait.
func (b usageBackoff) due(now time.Time) bool {
	return !now.Before(b.nextAttempt.Add(usageBackoffSafetyMargin))
}

// next advances the state after one rate-limited outcome.
func (b usageBackoff) next(now time.Time, retryAt time.Time) usageBackoff {
	streak := b.streak + 1
	return usageBackoff{streak: streak, nextAttempt: usageBackoffUntil(now, streak, retryAt)}
}

// usageRetryAt digs a 429's Retry-After out of an error, or returns the zero
// time when there is none to find.
func usageRetryAt(err error) time.Time {
	var httpErr *usageHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.RetryAt
	}
	return time.Time{}
}

// classifyUsageErr splits a failed usage fetch into "this credential is
// actually dead" and "this attempt didn't work". Only 401/403 is the former:
// the endpoint 429s readily (every Claude Code session shares the account's
// per-token budget), and a 5xx, a timeout, or an unreachable network say
// nothing at all about the token.
//
// reason is a short fixed tag, never the underlying error's text. A network
// failure stringifies as a *url.Error carrying the full request URL, which is
// noise in a one-line header placeholder and would leak into the /usage JSON;
// the tags below are the whole vocabulary.
func classifyUsageErr(err error) (expired bool, reason string) {
	if err == nil {
		return false, ""
	}
	var httpErr *usageHTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden:
			return true, usageExpiredReason
		case httpErr.Status == http.StatusTooManyRequests:
			return false, usageRateLimitedReason
		case httpErr.Status >= 500:
			return false, "server error"
		default:
			return false, fmt.Sprintf("HTTP %d", httpErr.Status)
		}
	}
	// fetchUsageInfo's own http.Client{Timeout} expires as a *url.Error wrapping
	// a context deadline — net.Error.Timeout() is what distinguishes it from a
	// DNS failure or a refused connection.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false, "timed out"
	}
	return false, "unreachable"
}

// usageRefreshInterval is how often the background poller refetches. Usage
// percentages move slowly; 2 minutes keeps the bar fresh without hammering
// the endpoint.
const usageRefreshInterval = 2 * time.Minute

// usageCoalesceWindow bounds how recently another process (or this same
// process's own prior pass) must have fetched an account for a pass that
// would otherwise make a real request to skip it and re-serve that reading
// as fresh instead. Deliberately half of usageRefreshInterval: a lone
// process's own next natural tick lands one full usageRefreshInterval after
// its own fetch, comfortably outside this window, so it never coalesces
// against itself (see TestUsageFetcherDoesNotSelfCoalesceAtItsOwnNextTick) —
// only two fetches landing close together (a launch burst, or two processes
// whose tickers happen to be closely phased) ever trigger it. An earlier
// version of this design used the full interval and was rejected on
// independent review for exactly this self-throttle failure mode.
//
// One consequence worth naming, because it looks like a bug and is not: this is
// also why usagePoller.Resume's immediate re-fetch kick — the one that fires on
// returning from an interactive program — can now be answered by a reading from
// within the last minute instead of a real request. That is the desirable
// direction. A resume/attach cycle is exactly the moment a burst of fresh
// requests is least justified (the numbers cannot have moved far in a minute)
// and most likely (attach, detach, attach again), so coalescing it away is the
// kick behaving well rather than being suppressed.
const usageCoalesceWindow = usageRefreshInterval / 2

// usageRetryMin seeds the failed-fetch backoff. The endpoint 429s readily
// (every Claude Code session shares the account's per-token budget), so a
// failed fetch retries at 5s, 10s, 20s… capped at the refresh interval,
// instead of leaving the header bar blank until the next 2-minute tick.
const usageRetryMin = 5 * time.Second

// usageCacheMaxAge bounds how stale a disk-cached Codex snapshot may be and
// still seed the header on startup (codex_usage.go's loadCodexUsageCache) —
// its only remaining use. The Anthropic side (live account + known accounts)
// used to share this bound for both its disk seed and its live carry-forward;
// both are deliberately unbounded instead — see liveCarryable and fresh —
// because the alternative was a bare "rate limited" placeholder replacing
// perfectly informative (if aging) bars the moment an outage ran long, which
// is worse than a number that says how old it is.
const usageCacheMaxAge = 15 * time.Minute

// UsageHub polls the Anthropic OAuth usage endpoint in the background so the
// render loop never blocks on credentials or the network (see usagePoller for
// the shared mechanism it delegates to). It holds the account-paired snapshot
// (AccountUsage) — the account email is re-read every fetch, like the token, so
// a mid-run relogin re-attributes the limits. The public surface — NewUsageHub,
// Snapshot, Pause, Resume, Kick, Shutdown — is unchanged.
type UsageHub = usagePoller[AccountUsage]

// liveCarryable reports whether the live account's last good reading may be
// re-served in place of a fetch the backoff is holding. It is carryable
// (known_accounts.go) in the one-account shape: same numbers-and-info test as
// fresh, plus the same identity test, against the email that is live right now.
//
// The identity half is what carryable documents at length — last is keyed by
// nothing at all, it is simply "whoever was live last time", and fetchUsage
// re-reads the email every fetch precisely so a switch re-attributes the
// limits. An unconfirmable email on either side never carries: putting an
// account's bars on screen means knowing whose they are.
//
// There is deliberately no age bound: numbers from an outage that ran long are
// still more informative than a bare "rate limited" placeholder, and the Stale
// marker (never cleared by a carry) already tells the header, and
// localFreshAccountEmails, that these are not current — the age itself doesn't
// need to gate anything on top of that. The explicit zero check is still what
// stops a disk seed written before AccountUsage carried a timestamp from
// reading as carryable — an unstamped reading's vintage is unknown, not merely
// old — the same one-time drop known_accounts.go's fresh() takes for its own
// pre-timestamp entries.
//
// last.Account is the profile-verified email when the profile endpoint answered,
// so the identity test now compares "whose numbers these actually are" against
// "who this host believes is logged in". A standing disagreement — the clobber
// signature — therefore stops the carry rather than re-serving numbers under an
// identity this host cannot confirm, which is the same conclusion carryable
// reaches for a snapshot whose name has been reused.
func liveCarryable(last *AccountUsage, live string) bool {
	return last != nil && last.Info != nil && !last.FetchedAt.IsZero() &&
		last.Account != "" && live != "" && strings.EqualFold(last.Account, live)
}

// newUsageFetcher wraps a live-account usage fetch with the same consecutive-429
// backoff newKnownAccountsFetcher applies per snapshot account — the one-account
// shape of it, so a single usageBackoff replaces that closure's map. The live
// account is the one every Claude Code session on this host is actually
// spending, so it is the account most likely to be throttled, and
// usagePoller.run's generic retry (5s doubling to usageRefreshInterval) would
// otherwise answer a 429 with roughly six more requests in the first four
// minutes, against the endpoint already saying stop.
//
// That generic schedule is deliberately left alone — CodexUsageHub shares it and
// it is the right answer for a genuine transient (a network blip, a Keychain
// hiccup). So a skipped pass is reported as an ordinary *success* re-serving
// last, not an error: an error would engage exactly the retry storm this exists
// to prevent, while a success simply leaves the poller on its 2-minute cadence
// and lets this closure's own due() gate decide whether the next call is a real
// round trip. Nothing observable changes for the header either way — the poller
// already keeps the previous value visible across a failed refresh.
//
// Unlike the version of this function that predates account_cache.go, there is
// no in-memory state to speak of on the common path — every pass resolves
// which account is live (resolveActiveSnapshotName) and loads THAT account's
// own cache entry fresh from disk (loadAccountCache), the same cost
// newKnownAccountsFetcher already pays every pass to resolve names. That is
// what makes an account switch a non-event instead of a special case: switch
// to an account that was previously tracked as "known" and this pass
// resolves straight to its existing slot — same numbers, same backoff wait,
// no special-casing needed to detect the switch happened. Before this, the
// live and known-account caches were separate files per role, so switching
// lost continuity in both directions — the incoming account started its
// numbers over, and the outgoing account lost its backoff streak the moment
// it reappeared on the known side. See CLAUDE.md's "Usage polling" section.
//
// A re-serve is gated on entryIdentityMatches/liveCarryable — the loaded
// entry's account must still be the one that is live. This gates BOTH halves
// of the entry, not just the numbers: an independent review caught an
// earlier version of this function extracting the loaded backoff
// unconditionally, so a claude-switch snapshot name reassigned to a
// different account (`account save --force`) would inherit the outgoing
// account's armed wait even though the new account was never itself
// throttled. Re-serving numbers is a *copy* marked Stale, so the loaded
// reading's own FetchedAt is preserved (restamping it would let a
// permanently throttled account look permanently fresh, the failure
// KnownAccountUsage avoids the same way); its Verified provenance is carried
// from the loaded entry's own field, never recomputed from
// UsageInfo.VerifiedAccount on a re-serve — that field is json:"-" and reads
// as empty on anything that went through a JSON round trip, which silently
// downgraded Verified to false on every carried/failed pass until an
// independent review caught that too.
//
// When there is nothing safe to re-serve — no cached entry at all (a cold
// start, or an account that has never once succeeded), or an identity
// mismatch — an armed wait returns a bare identity placeholder (the live
// email, no Info) as a success, and still does not fetch. There is no age
// condition here: once a cached reading exists and the identity checks out, it
// carries regardless of how long the backoff has been holding it. This is the
// whole point of the wait: falling through to a real fetch here was a no-op
// backoff, and it fired in exactly the case the mechanism exists for — a fresh
// TUI launch against an already-throttled account, where the 429 became an
// error and usagePoller.run answered it with its own 5s-doubling retry, the
// burst this is meant to prevent.
//
// A loaded deadline further out than usageBackoffCeiling is clamped by
// loadAccountCache, which reports whether it did so (clamped) rather than
// writing the correction back itself — this closure holds the injected save
// seam, loadAccountCache doesn't, so the self-heal write happens here.
//
// Two cases have no disk slot to work with at all — an unconfirmable identity
// (loadAccountEmail returns "") and a live account that has never been
// `account save`d (resolveActiveSnapshotName returns "") — and both fall
// back to a small amount of in-memory state (fbLast/fbBackoff/fbArmedFor)
// instead, mirroring the design this function had before account_cache.go
// existed. This fallback is NOT optional polish: an earlier version of this
// rewrite had no in-memory state at all, so an account with no snapshot got
// a fresh, un-armed backoff on literally every single pass — not just after
// a restart — meaning consecutive throttles never built past streak 1 and
// the endpoint got hit every usageRefreshInterval tick forever, exactly the
// hammering this whole mechanism exists to prevent. Independent review
// caught it with an A/B fetch-count comparison against main.
//
// The fallback vars are also kept in sync on the disk path, at every one of
// its return points, even though disk is authoritative whenever a name
// resolves — this is what lets a wait armed while identity WAS readable
// still hold once identity stops being readable mid-run (loadAccountEmail
// starts returning "", or a snapshot's account.json is deleted): the
// fallback already knows about it from the last pass that could see disk,
// instead of starting blank and letting an armed wait go unenforced the
// moment the file read breaks. Independent review's own reproduction for
// this test case is what surfaced the gap.
//
// The fallback also does NOT refuse unconditionally on an unconfirmable
// identity, unlike an earlier version of this function — it refuses only
// while a wait is actually armed, exactly like the disk path. Refusing
// unconditionally assumed the file read is the only route to identity, but
// fetchVerifiedUsageInfo's profile probe can attribute numbers to a verified
// account the file alone couldn't confirm (see usageAccountLabel) — so an
// unconditional refusal would permanently blank the header the instant
// ~/.claude.json becomes unreadable, even though a real fetch might recover
// identity through the token itself. Independent review caught this too.
//
// fetch is a parameter rather than a package var so a test can drive the whole
// state machine with a counter and no seam into the network or the Keychain
// (the same reason newKnownAccountsFetcher's per-account fetch is injected).
// save is injected for the identical reason: persisting straight to
// accountCachePath from inside the closure would make every test that
// exercises this path write to the real host's /tmp (several set HOME via
// loginAs but not TMPDIR), the exact class of bug keychainRead's
// TestMain-panic seam exists to catch. NewUsageHub passes the real
// saveAccountCache; tests pass a no-op or a spy.
// fbUnconfirmedOwner marks a fallback wait armed while identity was
// unconfirmable (live == ""), as opposed to fbArmedFor == "" which means
// nothing is armed at all. Without this distinction a wait armed with no
// readable identity would look indistinguishable from "untracked" to
// mirrorFallback's guard, and a later resolved account's disk pass could
// clobber it — caught by independent review, no repro needed once named:
// coalesceOwner is what makes fbArmedFor never legitimately equal "" while
// something is armed, so "" reliably means untracked everywhere it's read.
const fbUnconfirmedOwner = "\x00unconfirmed"

func coalesceOwner(live string) string {
	if live == "" {
		return fbUnconfirmedOwner
	}
	return live
}

func newUsageFetcher(fetch func() (*AccountUsage, error), save func(name string, e accountCacheEntry)) func() (*AccountUsage, error) {
	var fbLast *AccountUsage
	var fbBackoff usageBackoff
	var fbArmedFor string

	return func() (*AccountUsage, error) {
		now := usageClockNow()
		live := loadAccountEmail()
		name := ""
		if live != "" {
			name, _ = resolveActiveSnapshotName(live)
		}

		if name == "" {
			if live != "" && fbArmedFor != "" && !strings.EqualFold(fbArmedFor, live) {
				// A different account is logged in now than the one this
				// fallback wait was armed against. live == "" (identity
				// unreadable) never clears here — an unreadable identity
				// isn't evidence of a *different* one, and clearing on it
				// would drop a wait mirrored from a real, now-unreadable
				// identity right when it's needed most.
				fbLast, fbBackoff, fbArmedFor = nil, usageBackoff{}, ""
			}
			if !fbBackoff.due(now) {
				switch {
				case live != "" && liveCarryable(fbLast, live):
					carried := *fbLast
					carried.Stale = true
					return &carried, nil
				default:
					return &AccountUsage{Account: live}, nil
				}
			}
			u, err := fetch()
			if err != nil {
				if _, reason := classifyUsageErr(err); reason == usageRateLimitedReason {
					fbBackoff = fbBackoff.next(now, usageRetryAt(err))
					fbArmedFor = coalesceOwner(live)
				} else {
					fbBackoff, fbArmedFor = usageBackoff{}, ""
				}
				return nil, err
			}
			fbLast = u
			fbBackoff, fbArmedFor = usageBackoff{}, coalesceOwner(live)
			return u, nil
		}

		// A snapshot slot resolved: disk is authoritative from here, which is
		// what gives switching accounts its continuity. Every return point
		// below also mirrors its outcome into the fbXXX fallback vars — not
		// because the fallback needs it while a name keeps resolving, but so
		// that IF identity later becomes unconfirmable (name stops resolving
		// mid-run), the fallback path above already knows about a wait that
		// was armed while identity was still readable, instead of starting
		// blank and letting an armed wait go unenforced the moment
		// ~/.claude.json becomes unreadable. The mirror only fires when the
		// fallback is untracked or already tracking THIS live email — a
		// snapshotted account's disk pass must never clobber a *different*,
		// still-unsnapshotted account's own armed wait (proven by
		// TestUsageFetcherFallbackSurvivesAnUnrelatedDiskPass: without this
		// guard, switching away to a snapshotted account and back loses the
		// unsnapshotted account's streak).
		mirrorFallback := func(b usageBackoff, u *AccountUsage) {
			if fbArmedFor == "" || strings.EqualFold(fbArmedFor, live) {
				fbBackoff, fbArmedFor, fbLast = b, live, u
			}
		}
		cached, clamped := loadAccountCache(name)
		if clamped {
			save(name, cached)
		}
		matches := entryIdentityMatches(cached, live)
		backoff := cached.backoff()
		var last *AccountUsage
		switch {
		case matches && cached.Info != nil:
			last = &AccountUsage{Account: cached.Account, Info: cached.Info, Stale: cached.Stale, FetchedAt: cached.FetchedAt}
		case !matches && cached.Account != "":
			// Neither half of a mismatched entry belongs to whoever is live
			// now — see entryIdentityMatches.
			backoff = usageBackoff{}
		}

		persist := func(b usageBackoff, u *AccountUsage, verified bool) {
			e := accountCacheEntry{BackoffStreak: b.streak, BackoffNextAttempt: b.nextAttempt}
			if u != nil {
				e.Account, e.FetchedAt, e.Info, e.Verified = u.Account, u.FetchedAt, u.Info, verified
			}
			save(name, e)
			mirrorFallback(b, u)
		}

		if !backoff.due(now) {
			mirrorFallback(backoff, last)
			switch {
			case liveCarryable(last, live):
				// A copy, never the loaded value: last must keep its original
				// FetchedAt so the carry stays bounded, and must not itself become
				// stale — a later real fetch replaces it wholesale anyway.
				carried := *last
				carried.Stale = true
				return &carried, nil
			default:
				// Nothing safe to re-serve. Identity only, no numbers, and no
				// request — see the doc comment.
				return &AccountUsage{Account: live}, nil
			}
		}
		// Read-before-fetch coalescing: another process (or this one, a
		// moment ago) already has a reading recent enough that a fetch here
		// would be redundant. liveCarryable is the same identity gate the
		// carry-forward branch above already trusts, so nothing is served
		// here that wasn't already safe to re-serve — this only changes HOW
		// recently trusted, not WHETHER. No persist: disk already reflects
		// this reading. See usageCoalesceWindow's doc comment for why a lone
		// process's own next tick can never trigger this.
		if liveCarryable(last, live) && !last.FetchedAt.After(now) && now.Sub(last.FetchedAt) < usageCoalesceWindow {
			coalesced := *last
			coalesced.Stale = false
			mirrorFallback(backoff, &coalesced)
			return &coalesced, nil
		}
		u, err := fetch()
		if err != nil {
			// Only a throttle builds the streak; every other failure clears it, the
			// same rule the known-accounts scheduler follows — the wait is an answer
			// to being rate limited, not to being broken. last (the numbers, if any)
			// carries forward unchanged either way — only the backoff half changed.
			if _, reason := classifyUsageErr(err); reason == usageRateLimitedReason {
				backoff = backoff.next(now, usageRetryAt(err))
			} else {
				backoff = usageBackoff{}
			}
			persist(backoff, last, matches && cached.Verified)
			return nil, err
		}
		persist(usageBackoff{}, u, u.Info != nil && u.Info.VerifiedAccount != "")
		return u, nil
	}
}

// NewUsageHub starts the poller and returns immediately; the first fetch is
// kicked off asynchronously. A recent disk-cached entry for whichever account
// is live right now seeds the header so a restart while the endpoint is
// throttling still shows a (stale) bar without waiting on the fetcher's own
// first pass, which reaches the identical cache entry a moment later anyway.
func NewUsageHub() *UsageHub {
	var seed *AccountUsage
	if name, _ := resolveActiveSnapshotName(loadAccountEmail()); name != "" {
		if e, _ := loadAccountCache(name); e.Info != nil {
			seed = &AccountUsage{Account: e.Account, Info: e.Info, Stale: e.Stale, FetchedAt: e.FetchedAt}
		}
	}
	return newUsagePoller(seed, newUsageFetcher(fetchUsage, saveAccountCache), func(*AccountUsage) {})
}
