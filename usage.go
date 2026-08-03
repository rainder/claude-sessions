package main

import (
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
	// built — a carried-forward reading keeps the original timestamp, which is
	// what bounds how long staleness may accumulate (usageCacheMaxAge). Only
	// fetchUsage sets it; the server's on-demand /usage handler has no memory to
	// carry anything forward, so it never produces a Stale reading either.
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
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse credentials: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no access token in credentials")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

// fetchUsage hits the usage endpoint with the current token and pairs the parsed
// snapshot with a fresh read of the logged-in account email (loadAccountEmail),
// so a relogin into a different account mid-run re-attributes the limits instead
// of reporting them under the account read at startup. The email is read at
// fetch time, like the token; an unreadable email yields Account "" (not an
// error). The HTTP leg has a 5s timeout; token loading (macOS Keychain) and the
// email read are off the render path in the poller's background goroutine.
//
// This is the only place AccountUsage.FetchedAt is stamped: it dates the numbers
// themselves, so everything that re-serves them (newUsageFetcher) carries the
// original timestamp forward rather than restamping it.
func fetchUsage() (*AccountUsage, error) {
	tok, err := loadOAuthToken()
	if err != nil {
		return nil, err
	}
	info, err := fetchUsageInfo(tok)
	if err != nil {
		return nil, err
	}
	return &AccountUsage{Account: loadAccountEmail(), Info: info, FetchedAt: time.Now()}, nil
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
// throttling. The first 429 is free (throttles usually heal within one tick);
// only a *consecutive* second one starts thinning the retries.
const (
	// usageBackoffSecond is the wait after two consecutive 429s.
	usageBackoffSecond = 4 * time.Minute
	// usageBackoffMax is the wait after three or more — the schedule caps here
	// rather than growing, so an account that heals is picked up again within
	// one cap window instead of an ever-lengthening one.
	usageBackoffMax = 8 * time.Minute
	// usageBackoffCeiling bounds a Retry-After the endpoint asks for. A bogus or
	// absurd header value must not be able to wedge an account out of the
	// rotation for the life of the process.
	usageBackoffCeiling = 15 * time.Minute
)

// usageBackoffUntil returns when an account whose last streak consecutive
// fetches were rate limited may be tried again. streak <= 1 returns now — the
// first 429 imposes no wait at all, so the next ordinary tick fetches normally.
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

// usageBackoff is one account's consecutive-429 state. nextAttempt is always
// set (it equals the recording instant while streak is 1, i.e. "due now"), so
// "is this account backed off" is one comparison and nothing has to special-case
// a zero time.
type usageBackoff struct {
	streak      int
	nextAttempt time.Time
}

// due reports whether an account may be fetched again now.
func (b usageBackoff) due(now time.Time) bool { return !now.Before(b.nextAttempt) }

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
	// A fetch the server's cache declined to even attempt is reported as what it
	// is standing in for: the account is in backoff because it kept being
	// throttled, so the caller's row says "rate limited" exactly as it would have
	// said after the round trip this skipped.
	if errors.Is(err, errUsageBackoffActive) {
		return false, usageRateLimitedReason
	}
	if errors.Is(err, errUsageFetchTimedOut) {
		return false, "timed out"
	}
	// fetchUsageInfo's own http.Client{Timeout} expires as a *url.Error wrapping
	// a context deadline, not errUsageFetchTimedOut (that sentinel belongs to
	// usage_cache.go's separate runBounded watchdog) — net.Error.Timeout() is
	// what actually distinguishes it from a DNS failure or a refused connection.
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

// usageRetryMin seeds the failed-fetch backoff. The endpoint 429s readily
// (every Claude Code session shares the account's per-token budget), so a
// failed fetch retries at 5s, 10s, 20s… capped at the refresh interval,
// instead of leaving the header bar blank until the next 2-minute tick.
const usageRetryMin = 5 * time.Second

// usageCacheMaxAge bounds how stale a disk-cached snapshot may be and still
// seed the header on startup. Beyond this the percentages are more likely to
// mislead than inform, so the bar waits for a live fetch instead.
const usageCacheMaxAge = 15 * time.Minute

// usageCachePath is where the last successful fetch is persisted so a
// restart during an endpoint throttle still has something to show. UID in
// the name keeps multi-user /tmp collisions (and permission errors) away.
func usageCachePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("claude-sessions-usage-%d.json", os.Getuid()))
}

// cachedUsage is the on-disk envelope: the account-paired snapshot plus when it
// was fetched. The snapshot lives under "usage" (account + info); a pre-relogin
// cache that stored the bare snapshot under "info" decodes to a zero Usage (nil
// Info), which loadUsageCache treats as a miss.
type cachedUsage struct {
	FetchedAt time.Time    `json:"fetched_at"`
	Usage     AccountUsage `json:"usage"`
}

// saveUsageCache persists a successful fetch. Best-effort: a read-only /tmp
// just means no warm start next launch.
func saveUsageCache(u *AccountUsage) {
	data, err := json.Marshal(cachedUsage{FetchedAt: time.Now(), Usage: *u})
	if err != nil {
		return
	}
	_ = os.WriteFile(usageCachePath(), data, 0600)
}

// loadUsageCache returns the cached snapshot, or nil if absent, unreadable,
// older than usageCacheMaxAge, or missing its snapshot (nil Info — including a
// pre-relogin cache written in the old "info" envelope, which is a miss, not an
// error).
func loadUsageCache() *AccountUsage {
	data, err := os.ReadFile(usageCachePath())
	if err != nil {
		return nil
	}
	var c cachedUsage
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if c.FetchedAt.IsZero() || time.Since(c.FetchedAt) > usageCacheMaxAge || c.Usage.Info == nil {
		return nil
	}
	return &c.Usage
}

// UsageHub polls the Anthropic OAuth usage endpoint in the background so the
// render loop never blocks on credentials or the network (see usagePoller for
// the shared mechanism it delegates to). It holds the account-paired snapshot
// (AccountUsage) — the account email is re-read every fetch, like the token, so
// a mid-run relogin re-attributes the limits. The public surface — NewUsageHub,
// Snapshot, Pause, Resume, Kick, Shutdown — is unchanged.
type UsageHub = usagePoller[AccountUsage]

// liveCarryable reports whether the live account's last good reading may be
// re-served in place of a fetch the backoff is holding. It is carryable
// (known_accounts.go) in the one-account shape: same numbers-and-age test as
// fresh, plus the same identity test, against the email that is live right now.
//
// The identity half is what carryable documents at length — last is keyed by
// nothing at all, it is simply "whoever was live last time", and fetchUsage
// re-reads the email every fetch precisely so a switch re-attributes the
// limits. An unconfirmable email on either side never carries: putting an
// account's bars on screen means knowing whose they are.
//
// The age half is what stops a throttle that never lifts from showing hours-old
// percentages as current, and it is what localFreshAccountEmails needs in order
// to stop telling every remote host to skip an account nobody has current
// numbers for. The explicit zero check is also how a disk seed written before
// AccountUsage carried a timestamp reads as non-carryable rather than eternally
// young — the same one-time drop loadKnownAccountsCache takes for its own
// pre-timestamp entries.
func liveCarryable(last *AccountUsage, live string) bool {
	return last != nil && last.Info != nil && !last.FetchedAt.IsZero() &&
		time.Since(last.FetchedAt) <= usageCacheMaxAge &&
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
// A re-serve is gated on liveCarryable — the account must still be the one that
// is live, and its numbers must still be young enough to inform. Re-serving is a
// *copy* marked Stale, so the stored last keeps its original FetchedAt and the
// carry stays bounded (restamping it would let a permanently throttled account
// look permanently fresh, the failure KnownAccountUsage avoids the same way),
// and downstream can tell carried numbers from live ones.
//
// When there is nothing safe to re-serve — no last at all (a cold start, or an
// account that has never once succeeded), an identity that cannot be confirmed,
// or numbers past usageCacheMaxAge — an armed wait returns a bare identity
// placeholder (the live email, no Info) as a success, and still does not fetch.
// This is the whole point of the wait: falling through to a real fetch here was
// a no-op backoff, and it fired in exactly the case the mechanism exists for —
// a fresh TUI launch against an already-throttled account, where the 429 became
// an error and usagePoller.run answered it with its own 5s-doubling retry, the
// burst this is meant to prevent. That path had no equivalent on the
// known-accounts side, whose batch fetch never returns an error for one
// account's failure and so can never wake the generic retry.
//
// An unconfirmable identity used to fetch rather than re-serve, on the grounds
// that showing an account's bars means knowing whose they are. The placeholder
// is a third option that did not exist then and dominates both: it shows no
// numbers at all, so there is nothing to misattribute, and it costs no request.
// A *confirmed* switch is the one case that still forces a fetch — the wait was
// armed against the previous account's budget and the numbers on hand are not
// this one's, so both memories are dropped and this pass has nothing whatsoever
// to say about the new account until it asks.
//
// Switch detection cannot be keyed off last.Account: last is nil until the
// first success, and can hold an empty Account forever if identity was
// unreadable at the moment that success landed — either way it would leave the
// wait permanently unable to recognise a switch, silently swallowing a Ctrl+W
// kick for the rest of the backoff window. armedFor is tracked independently —
// the email that was live at the moment the CURRENT streak was armed or
// extended — precisely so switch detection works whether or not last carries a
// usable identity of its own.
//
// fetch is a parameter rather than a package var so a test can drive the whole
// state machine with a counter and no seam into the network or the Keychain
// (the same reason allKnownAccounts takes its per-account fetch as one). last,
// backoff and armedFor are touched only inside the returned closure, which
// usagePoller.run calls one at a time from its single goroutine — the same
// single-owner rule newKnownAccountsFetcher's maps rely on, so neither needs a
// lock.
func newUsageFetcher(seed *AccountUsage, fetch func() (*AccountUsage, error)) func() (*AccountUsage, error) {
	last := seed
	var backoff usageBackoff
	armedFor := ""
	if seed != nil {
		armedFor = seed.Account
	}
	return func() (*AccountUsage, error) {
		now := time.Now()
		live := loadAccountEmail()
		if !backoff.due(now) {
			switch {
			case live != "" && armedFor != "" && !strings.EqualFold(armedFor, live):
				// A different account is logged in now than the one this wait was
				// armed against — that budget says nothing about this account, so
				// both memories drop and this pass asks for real.
				last, backoff, armedFor = nil, usageBackoff{}, ""
			case liveCarryable(last, live):
				// A copy, never the stored pointer: last must keep its original
				// FetchedAt so the carry stays bounded, and must not itself become
				// stale — a later real fetch replaces it wholesale anyway.
				carried := *last
				carried.Stale = true
				return &carried, nil
			default:
				// Nothing safe to re-serve. Identity only, no numbers, and no
				// request — see the doc comment. last is deliberately left where it
				// is: a too-old reading is not a wrong one, a success overwrites it,
				// and clearing it would buy nothing.
				return &AccountUsage{Account: live}, nil
			}
		}
		u, err := fetch()
		if err != nil {
			// Only a throttle builds the streak; every other failure clears it, the
			// same rule the known-accounts scheduler follows — the wait is an answer
			// to being rate limited, not to being broken.
			if _, reason := classifyUsageErr(err); reason == usageRateLimitedReason {
				backoff = backoff.next(now, usageRetryAt(err))
				armedFor = live
			} else {
				backoff, armedFor = usageBackoff{}, ""
			}
			return nil, err
		}
		last = u
		backoff, armedFor = usageBackoff{}, u.Account
		return u, nil
	}
}

// saveOnceUsage keeps a re-served snapshot out of the disk cache. saveUsageCache
// stamps FetchedAt with time.Now(), and the poller saves on every success — so
// without this, newUsageFetcher's skipped passes would rewrite the cache every
// two minutes with numbers that never moved, and usageCacheMaxAge would stop
// bounding anything: a warm start during a long throttle would seed the header
// with arbitrarily old percentages presented as current. That is precisely the
// failure KnownAccountUsage avoids by carrying prev's *original* FetchedAt
// forward rather than restamping it.
//
// Only a genuinely fetched reading reaches disk, which takes three tests. A
// carried-forward reading is a fresh *copy* each pass, so pointer identity alone
// no longer recognises it — Stale is what names it, and persisting one would
// restamp the envelope with numbers that never moved, exactly the failure above.
// A placeholder (no Info) is worse than useless on disk: loadUsageCache reads a
// nil-Info entry as a miss, so writing one over a good cache file would not just
// stale the warm start but destroy it. And identity still separates a re-served
// pointer from a fetched one with no assumption about whether two fetches that
// happen to agree should count as one; seed is the same snapshot the fetcher
// starts as its last, so a first pass that lands mid-throttle re-serves it
// without rewriting the cache entry it came from.
func saveOnceUsage(seed *AccountUsage, save func(*AccountUsage)) func(*AccountUsage) {
	saved := seed
	return func(u *AccountUsage) {
		if u == saved || u == nil || u.Info == nil || u.Stale {
			return
		}
		saved = u
		save(u)
	}
}

// NewUsageHub starts the poller and returns immediately; the first fetch is
// kicked off asynchronously. A recent disk-cached snapshot seeds the header so
// a restart while the endpoint is throttling still shows a (stale) bar — and
// seeds the fetcher and the save wrapper alongside it, so a pass skipped by the
// backoff re-serves that snapshot without restamping it on disk.
func NewUsageHub() *UsageHub {
	seed := loadUsageCache()
	return newUsagePoller(seed, newUsageFetcher(seed, fetchUsage), saveOnceUsage(seed, saveUsageCache))
}
