package main

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// usageCacheTTL is how long a successful account usage fetch is replayed to
// later callers of GET /usage. Deliberately a little under usageRefreshInterval
// (the cadence the background poller this cache replaced used to fetch at, and
// the cadence RemoteUsageHub polls this endpoint at): a cache entry's clock
// starts when its fetch *completes*, strictly after the poll tick that
// triggered it, so a TTL exactly equal to the poll interval means the very next
// tick almost always lands just before expiry and reuses a stale answer instead
// of refreshing — doubling the effective staleness in the steady state. Sitting
// comfortably under the interval instead means every tick's request is a
// genuine cache miss.
const usageCacheTTL = 100 * time.Second

// usageFetchTimeout bounds a single account fetch inside GetOrFetch,
// independent of whatever the underlying fetch function does. Without this, a
// fetch that never returns — a locked macOS Keychain prompting a SecurityAgent
// dialog nothing will ever answer in a headless launchd/systemd session
// (loadOAuthToken, usage.go), or a wedged filesystem read (snapshotToken) —
// would leave that email's flight permanently unfinished: expired/prune both
// treat an unfinished flight as never-expired (the same rule that correctly
// protects a genuinely in-flight fetch from being evicted out from under its
// joiners), so every later GET /usage call for that account would park a new
// goroutine on the same flight's done channel forever, and the account would
// never be fetchable again for the life of the process — a strictly worse
// failure mode than the always-on poller this replaced, which just left one
// snapshot stale while every other request kept working. Bounding the fetch
// guarantees the flight always finishes, publishing a timeout error that the
// short usageCacheFailTTL then lets a later caller retry — self-healing if the
// underlying hang was transient, and at worst one leaked goroutine every
// usageCacheFailTTL for a persistently wedged account, not one per client per
// usageRefreshInterval forever.
const usageFetchTimeout = 10 * time.Second

// errUsageFetchTimedOut is published when a fetch outlives usageFetchTimeout.
var errUsageFetchTimedOut = errors.New("usage fetch timed out")

// usageCacheFailTTL is the same for a failed fetch, and it is deliberately
// short. spawnDedupe forgets a failure immediately so a retry genuinely re-runs;
// here the opposite is wanted. The usage endpoint 429s readily (every Claude
// Code session shares the account's per-token budget — see usageRetryMin), and
// a burst of clients arriving during a throttle would otherwise each re-trigger
// a fetch for the same account and deepen it. A brief negative-cache window is
// the request-driven equivalent of the poller's backoff, without reimplementing
// exponential backoff for a model that has no ticker to back off from.
const usageCacheFailTTL = 15 * time.Second

// errNoUsageResult stands in for a fetch that returned neither a snapshot nor an
// error. Nothing in production does that — but a panic inside fetch publishes
// through the deferred finish with both fields still zero, and a joiner reading
// (nil, nil) as success would dereference the nil. Publishing an error instead
// keeps "err == nil implies info != nil" true for every caller.
var errNoUsageResult = errors.New("usage fetch produced no result")

// usageFlight is one account's in-flight-or-remembered fetch: the channel
// joiners wait on, the outcome, and when it landed. finishedAt is zero while the
// fetch is still running, which is also what keeps expiry off it — dropping an
// entry a joiner is parked on would turn the join into a second fetch of the
// same account, exactly what the single-flight exists to prevent.
type usageFlight struct {
	done       chan struct{}
	info       *UsageInfo
	err        error
	finishedAt time.Time
}

// usageCache is the per-account single-flight + TTL cache behind GET /usage,
// modeled on spawnDedupe: same map-of-flights shape, same claim/join/publish
// pattern. It is keyed by lowercased account email, so two hosts' requests for
// the same account collapse into one Anthropic round trip, and a repeat within
// the TTL costs nothing at all.
//
// There is no size cap. The key space is "accounts this host holds a
// claude-switch snapshot for", which is a handful and bounded by the
// filesystem; the pruning on insert exists so a renamed or removed snapshot
// stops occupying an entry for the life of the process, not to enforce a bound.
//
// Keying by email rather than by snapshot name means two differently-named
// snapshots that happen to hold the same email (e.g. a renamed or duplicated
// account) share one fetch and one outcome — if their underlying tokens have
// since diverged (one revoked, one still good), both report whichever token's
// result won the race, not each its own. `allKnownAccounts` (known_accounts.go)
// fetched every name independently before this change. Accepted: two snapshots
// for the identical account is an unusual setup to begin with, and the
// alternative (keying by name) would refetch that account once per name even
// though it is the very same account — the more common cost.
type usageCache struct {
	mu      sync.Mutex
	entries map[string]*usageFlight
	// now is injectable so TTL expiry is testable without sleeping.
	now func() time.Time
	// fetchTimeout overrides usageFetchTimeout; zero means the default. Injectable
	// so the hang-protection test doesn't have to wait out the real timeout.
	fetchTimeout time.Duration
}

func (c *usageCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *usageCache) timeout() time.Duration {
	if c.fetchTimeout != 0 {
		return c.fetchTimeout
	}
	return usageFetchTimeout
}

func (c *usageCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// GetOrFetch returns email's usage snapshot, running fetch at most once per
// email per TTL. A caller arriving while a fetch is in flight waits for that
// one instead of starting its own.
//
// An empty email bypasses the cache entirely and always fetches. An account
// whose .account.json is missing or unreadable resolves to "" (see
// snapshotAccountEmail — an unknown email is a display detail, never a
// failure), so two *different* such accounts would otherwise collide on one key
// and be served each other's limits, fetched with the wrong token. That is rare
// enough (a corrupt identity file) that paying for an uncached fetch is the
// right trade against any scheme for synthesizing a key.
func (c *usageCache) GetOrFetch(email string, fetch func() (*UsageInfo, error)) (*UsageInfo, error) {
	key := strings.ToLower(email)
	if key == "" {
		return fetch()
	}
	c.mu.Lock()
	if existing := c.entries[key]; existing != nil {
		// Expiry is checked here as well as in prune, because prune runs on
		// insert and this lookup happens first.
		if !c.expired(existing, c.timeNow()) {
			c.mu.Unlock()
			<-existing.done
			return existing.info, existing.err
		}
		delete(c.entries, key)
	}
	c.prune()
	if c.entries == nil {
		c.entries = make(map[string]*usageFlight)
	}
	flight := &usageFlight{done: make(chan struct{})}
	c.entries[key] = flight
	c.mu.Unlock()

	var info *UsageInfo
	var err error
	// Publish through a defer so a panic in fetch cannot leave joiners parked on
	// done forever — the same reason POST /sessions/new publishes its spawn that
	// way.
	defer func() { c.finish(key, flight, info, err) }()
	info, err = runBounded(fetch, c.timeout())
	if err == nil && info == nil {
		err = errNoUsageResult
	}
	return info, err
}

// runBounded runs fetch and returns its result, or errUsageFetchTimedOut if it
// doesn't finish within timeout. A fetch that hangs past the timeout keeps
// running in its own goroutine — Go has no way to forcibly cancel it — but the
// caller (GetOrFetch) is unblocked either way, which is what keeps the cache
// entry from wedging forever. See usageFetchTimeout for why this exists.
func runBounded(fetch func() (*UsageInfo, error), timeout time.Duration) (*UsageInfo, error) {
	type result struct {
		info *UsageInfo
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		info, err := fetch()
		ch <- result{info, err}
	}()
	select {
	case r := <-ch:
		return r.info, r.err
	case <-time.After(timeout):
		return nil, errUsageFetchTimedOut
	}
}

// expired reports whether a finished flight has outlived its outcome's TTL. A
// running flight is never expired. Called under c.mu, which is also where
// finish writes err, so the branch below never races it.
func (c *usageCache) expired(f *usageFlight, now time.Time) bool {
	if f.finishedAt.IsZero() {
		return false
	}
	ttl := usageCacheTTL
	if f.err != nil {
		ttl = usageCacheFailTTL
	}
	return !now.Before(f.finishedAt.Add(ttl))
}

// prune drops every finished entry whose TTL has run out. Called under c.mu
// from GetOrFetch, on insert only — there is no background sweeper, so an
// account nobody asks about again simply keeps one small entry until the next
// insert notices it.
func (c *usageCache) prune() {
	now := c.timeNow()
	for key, flight := range c.entries {
		if c.expired(flight, now) {
			delete(c.entries, key)
		}
	}
}

// finish records the outcome and releases every joiner. Unlike spawnDedupe's,
// a failure is kept (see usageCacheFailTTL) rather than forgotten — nothing was
// created, so there is nothing to make a retry unsafe, only an endpoint to stop
// hammering.
func (c *usageCache) finish(key string, flight *usageFlight, info *UsageInfo, err error) {
	if err == nil && info == nil {
		err = errNoUsageResult
	}
	c.mu.Lock()
	flight.info = info
	flight.err = err
	flight.finishedAt = c.timeNow()
	c.mu.Unlock()
	close(flight.done)
}
