package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain makes the real macOS Keychain unreachable from this test binary.
//
// Every switch and save ends in `security add-generic-password -U -s "Claude
// Code-credentials"`, which would overwrite the live credential of whoever runs
// `go test`. Defaulting the seams to a panic means a test that forgets to
// install the fake fails loudly instead of destroying a real login; each test
// that needs them installs a tempdir-backed fake through newAccountFixture.
//
// The two real implementations (securityFindPassword / securityAddPassword) are
// therefore deliberately never executed by any test.
func TestMain(m *testing.M) {
	keychainRead = func() ([]byte, error) { panic("test reached the real Keychain (read)") }
	keychainWrite = func([]byte) error { panic("test reached the real Keychain (write)") }
	// Same fail-closed rule for the usage endpoint: a test that reaches the
	// poller's HTTP leg without swapFetch would spend the developer's own token
	// on a real request, so the default has to be loud rather than plausible.
	usageInfoFetch = func(string) (*UsageInfo, error) { panic("test reached the real usage endpoint") }
	oauthTokenRefresh = func(string, []string) (*oauthTokenResponse, error) {
		panic("test reached the real oauth token endpoint")
	}
	// Same rule for the identity probe beside it: a test that reaches the
	// profile endpoint would spend the developer's own token on a real request,
	// so the default is loud. Every path that probes gates on an unexpired
	// access token, which no fixture has unless it asks for one.
	profileEmailFetch = func(string) (string, error) { panic("test reached the real profile endpoint") }
	cuFetchFunc = func(context.Context, string) ([]byte, error) { panic("test reached the real cu CLI") }
	claudeSummarizeFunc = func(context.Context, string, []byte) ([]byte, error) { panic("test reached the real claude CLI") }
	codexSummarizeFunc = func(context.Context, string, []byte) ([]byte, error) { panic("test reached the real codex CLI") }
	// Hermetic default: without this, resolveSummarizeFunc would call the
	// real LoadSummaryBackend, reading whatever this developer's own
	// ~/.config/claude-sessions/summary-backend happens to say instead of
	// the "claude" every existing claudeSummarizeFunc override assumes.
	summaryBackendFunc = func() string { return "claude" }
	os.Exit(m.Run())
}

// accountFixture is a self-contained ~/.claude world: a temp HOME, and — on
// darwin — a fake Keychain backed by a file inside it, so the live credential
// behaves identically on both platforms and every byte a test writes stays
// inside t.TempDir().
type accountFixture struct {
	t        *testing.T
	home     string
	keychain string // darwin only: the file standing in for the Keychain item
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	f := &accountFixture{t: t, home: home, keychain: filepath.Join(home, ".fake-keychain")}
	prevRead, prevWrite := keychainRead, keychainWrite
	keychainRead = func() ([]byte, error) { return os.ReadFile(f.keychain) }
	keychainWrite = func(data []byte) error { return os.WriteFile(f.keychain, data, 0600) }
	t.Cleanup(func() { keychainRead, keychainWrite = prevRead, prevWrite })
	// A switch consults the live session list, and the real collector shells out
	// to `ps` (and `tmux`) on the developer's own machine. Nothing in these
	// fixtures has sessions, so default the seam to an empty list; the warning
	// tests override it afterwards with stubSwitchSessions.
	prevCollect := collectSessionsForSwitch
	collectSessionsForSwitch = func() ([]Session, error) { return nil, nil }
	t.Cleanup(func() { collectSessionsForSwitch = prevCollect })
	// Successful switches rotate the parked snapshot (and install the rotated
	// blob as live). A canned response keeps every existing switch test off
	// the real token endpoint; tests that need a failure or a no-call
	// override the seam afterwards.
	stubOAuthRefresh(t, fixtureOAuthRefresh)
	return f
}

// fixtureAccessForOrig is the access token fixtureOAuthRefresh writes for a
// snapshot whose original access token was orig (credBlob stores the refresh
// token as "refresh-"+orig). Tokens are per-refresh so two concurrent
// switches cannot both land on the same canned value and hide a torn
// credential/identity pair.
func fixtureAccessForOrig(orig string) string {
	return "rotated-access-refresh-" + orig
}

func fixtureOAuthRefresh(refresh string, _ []string) (*oauthTokenResponse, error) {
	return &oauthTokenResponse{
		AccessToken:           "rotated-access-" + refresh,
		RefreshToken:          "rotated-refresh-" + refresh,
		ExpiresIn:             3600,
		RefreshTokenExpiresIn: 86400,
	}, nil
}

func accessTokenOf(t *testing.T, blob string) string {
	t.Helper()
	tok, err := parseOAuthCredentials([]byte(blob))
	if err != nil {
		t.Fatalf("parse credential: %v (blob=%s)", err, blob)
	}
	return tok
}

// livePath is where the live credential physically lives for this platform: the
// fake Keychain item on darwin, ~/.claude/.credentials.json elsewhere. Tests
// read and write it directly so setup and assertions never depend on the code
// under test.
func (f *accountFixture) livePath() string {
	if runtime.GOOS == "darwin" {
		return f.keychain
	}
	return liveCredentialPath(f.home)
}

// setLive installs a credential blob as the account currently logged in.
func (f *accountFixture) setLive(token string) {
	f.t.Helper()
	if err := os.WriteFile(f.livePath(), []byte(credBlob(token)), 0600); err != nil {
		f.t.Fatal(err)
	}
}

// live returns the credential blob currently installed, or "" if there is none.
func (f *accountFixture) live() string {
	f.t.Helper()
	data, err := os.ReadFile(f.livePath())
	if err != nil {
		return ""
	}
	return string(data)
}

// setIdentity writes ~/.claude.json with the given login email plus a couple of
// unrelated keys, so every patch assertion can also prove the untouched keys
// survive.
func (f *accountFixture) setIdentity(email string) {
	f.t.Helper()
	cache := fmt.Sprintf(`{
	  "oauthAccount": {"emailAddress": %q, "organizationName": "org-%s"},
	  "userID": "uid-%s",
	  "projects": {"/tmp/x": {"history": ["one"]}},
	  "numStartups": 41
	}`, email, email, email)
	if err := os.WriteFile(identityCachePath(f.home), []byte(cache), 0600); err != nil {
		f.t.Fatal(err)
	}
}

// snapshot writes a credential+identity snapshot pair for one account name. The
// identity file holds only oauthAccount, like a snapshot claude-switch captured
// from a cache that had no userID yet.
func (f *accountFixture) snapshot(name, token, email string) {
	f.t.Helper()
	writeSnapshotFixture(f.t, f.home, name, token, email)
}

// snapshotCred returns one snapshot's credential blob, or "" when there is
// none — so an assertion can say "this file was not touched" without
// distinguishing an absent file from an unreadable one.
func (f *accountFixture) snapshotCred(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(snapshotCredentialPath(f.home, name))
	if err != nil {
		return ""
	}
	return string(data)
}

// snapshotIdentity replaces one snapshot's identity file with a full
// {oauthAccount, userID} slice, the shape both this tool and claude-switch's jq
// write.
func (f *accountFixture) snapshotIdentity(name, email, userID string) {
	f.t.Helper()
	data := fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q},"userID":%q}`, email, userID)
	if err := os.WriteFile(snapshotAccountPath(f.home, name), []byte(data), 0600); err != nil {
		f.t.Fatal(err)
	}
}

// credBlob is a credentials blob in the shape both the Keychain item and
// ~/.claude/.credentials.json hold — byte-identical to what
// writeSnapshotFixture parks in a snapshot, so "the live credential is now
// exactly that snapshot" is a plain string comparison.
//
// It carries the refresh token every real Claude Code credential has, because
// switchAccount refuses a snapshot without one (a credential that cannot
// refresh logs the host out the moment it ages out) — a fixture missing it
// would fail every switch test for a reason none of them is about.
//
// expiresAt is deliberately absent: the identity probe in
// validateSnapshotCredential only runs for a snapshot whose access token is
// still valid, so leaving it unset keeps every existing test off the network
// seam. credBlobAt below is for the tests that want that path.
func credBlob(token string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q}}`, token, "refresh-"+token)
}

// credBlobAt is credBlob with explicit expiries, in milliseconds since the
// epoch, for the validation tests. A zero for either field omits it, which is
// how "the file predates this field" is spelled.
func credBlobAt(token string, expiresAt, refreshExpiresAt time.Time) string {
	fields := fmt.Sprintf(`"accessToken":%q,"refreshToken":%q`, token, "refresh-"+token)
	if !expiresAt.IsZero() {
		fields += fmt.Sprintf(`,"expiresAt":%d`, expiresAt.UnixMilli())
	}
	if !refreshExpiresAt.IsZero() {
		fields += fmt.Sprintf(`,"refreshTokenExpiresAt":%d`, refreshExpiresAt.UnixMilli())
	}
	return fmt.Sprintf(`{"claudeAiOauth":{%s}}`, fields)
}

// treeState is a content+mtime fingerprint of every file under root, used to
// assert that an operation touched nothing at all. Content alone would miss a
// rewrite with identical bytes; mtime alone is noisy — together they are the
// portable form of "zero files written".
//
// The advisory lock file is excluded: taking the lock creates it, and it carries
// no data. "Zero files touched" is a claim about credentials and identity, not
// about the coordination artifact that guards them.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() == accountLockFile {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[rel] = hex.EncodeToString(sum[:8]) + "@" + info.ModTime().Format(time.RFC3339Nano)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := treeState(t, root)
	for name, state := range after {
		prev, ok := before[name]
		if !ok {
			t.Fatalf("file created: %s", name)
		}
		if prev != state {
			t.Fatalf("file modified: %s (%s -> %s)", name, prev, state)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Fatalf("file removed: %s", name)
		}
	}
}

// TestSwitchAccountUnknownName proves validation runs before anything else: an
// unknown name is an errUnknownAccount listing what this host does hold, and not
// one byte moves.
func TestSwitchAccountUnknownName(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	email, _, err := switchAccount("nope")
	if !errors.Is(err, errUnknownAccount) {
		t.Fatalf("err = %v, want errUnknownAccount", err)
	}
	if !strings.Contains(err.Error(), "avisoma") || !strings.Contains(err.Error(), "trecs") {
		t.Fatalf("err = %v, want the known names listed", err)
	}
	if email != "" {
		t.Fatalf("email = %q, want empty", email)
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountAlreadyActiveIsTrueNoOp proves that switching to the account
// already live touches zero files — no rescue backup, no re-applied snapshot.
// Re-writing "the same bytes" is not harmless: the live token can have refreshed
// since the snapshot was captured, so re-applying it would install a stale one.
func TestSwitchAccountAlreadyActiveIsTrueNoOp(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-stale", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setLive("tok-refreshed")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	email, _, err := switchAccount("avisoma")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if email != "andy@avisoma.com" {
		t.Fatalf("email = %q, want andy@avisoma.com", email)
	}
	assertTreeUnchanged(t, f.home, before)
	if got := f.live(); got != credBlob("tok-refreshed") {
		t.Fatalf("live credential = %q, want the untouched refreshed one", got)
	}
	if _, err := os.Stat(snapshotCredentialPath(f.home, rescueSnapshotName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rescue file exists after a no-op switch (stat err = %v)", err)
	}
}

// TestSwitchAccountRescuesUnidentifiedOutgoing covers the case the rescue slot
// exists for: the live email matches no snapshot, so the named sync-back cannot
// fire, and without an unconditional copy that credential would be overwritten
// with no backup anywhere.
func TestSwitchAccountRescuesUnidentifiedOutgoing(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.setLive("tok-stranger")
	f.setIdentity("someone@elsewhere.example")

	if _, _, err := switchAccount("avisoma"); err != nil {
		t.Fatalf("err = %v", err)
	}
	rescue, err := os.ReadFile(snapshotCredentialPath(f.home, rescueSnapshotName))
	if err != nil {
		t.Fatalf("rescue backup missing: %v", err)
	}
	if string(rescue) != credBlob("tok-stranger") {
		t.Fatalf("rescue = %q, want the outgoing live credential", rescue)
	}
	if got := accessTokenOf(t, f.live()); got != fixtureAccessForOrig("tok-avisoma") {
		t.Fatalf("live access token = %q, want the rotated blob", got)
	}
	if got := accessTokenOf(t, f.snapshotCred("avisoma")); got != fixtureAccessForOrig("tok-avisoma") {
		t.Fatalf("avisoma snapshot access = %q, want the rotated blob persisted", got)
	}
}

// TestSwitchAccountSyncsOutgoingAccount is the full happy path: the outgoing
// account's credential and identity are captured under its own name before the
// live credential is replaced, the incoming identity is patched into
// ~/.claude.json without disturbing anything else, and the returned email is
// re-read from disk.
func TestSwitchAccountSyncsOutgoingAccount(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma-old", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-andy@trecs.aero")
	f.setLive("tok-avisoma-refreshed")
	f.setIdentity("andy@avisoma.com")

	email, _, err := switchAccount("trecs")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if email != "andy@trecs.aero" {
		t.Fatalf("email = %q, want andy@trecs.aero", email)
	}

	// Step 4: the outgoing account keeps a copy of the credential that was
	// actually live, not the older one its snapshot held.
	outgoing, err := os.ReadFile(snapshotCredentialPath(f.home, "avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	if string(outgoing) != credBlob("tok-avisoma-refreshed") {
		t.Fatalf("outgoing snapshot = %q, want the refreshed live credential", outgoing)
	}
	identity, err := os.ReadFile(snapshotAccountPath(f.home, "avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	var slice map[string]json.RawMessage
	if err := json.Unmarshal(identity, &slice); err != nil {
		t.Fatal(err)
	}
	if len(slice) != 2 || slice["oauthAccount"] == nil || string(slice["userID"]) != `"uid-andy@avisoma.com"` {
		t.Fatalf("identity snapshot = %s, want exactly oauthAccount+userID", identity)
	}

	// Step 5: the live credential is the rotated target blob, and the
	// parked snapshot was rewritten to match so a later switch installs
	// the same grant.
	if got := accessTokenOf(t, f.live()); got != fixtureAccessForOrig("tok-trecs") {
		t.Fatalf("live access token = %q, want the rotated blob", got)
	}
	if got := accessTokenOf(t, f.snapshotCred("trecs")); got != fixtureAccessForOrig("tok-trecs") {
		t.Fatalf("trecs snapshot access = %q, want the rotated blob persisted", got)
	}

	// Step 6: the identity cache names the new account and keeps every key this
	// tool has no business touching.
	cache := readIdentityCache(t, f.home)
	if got := string(cache["userID"]); got != `"uid-andy@trecs.aero"` {
		t.Fatalf("userID = %s, want the incoming account's", got)
	}
	if _, ok := cache["projects"]; !ok {
		t.Fatalf("patch dropped the unrelated projects key: %v", keysOf(cache))
	}
	if got := string(cache["numStartups"]); got != "41" {
		t.Fatalf("numStartups = %s, want 41", got)
	}
	// A successful switch must disarm the pending marker — nothing left to
	// guard once both halves of the credential/identity pair completed.
	if got := readPendingSwitchMarker(f.home); got != "" {
		t.Fatalf("pending marker = %q after a clean switch, want none armed", got)
	}
}

// TestSwitchAccountRefusesWithPendingMarker simulates a crash between
// installing the live credential and patching ~/.claude.json (the one gap
// this tool cannot make atomic — two separate writes to two separate stores).
// The marker this leaves behind must block every subsequent switch, not just
// a retry of the same target: the natural instinct is "that didn't work, try
// again," and doing so with a stale identity cache would misattribute the
// outgoing backup to the wrong snapshot and corrupt it.
func TestSwitchAccountRefusesWithPendingMarker(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshot("third", "tok-third", "andy@third.example")
	f.setLive("tok-trecs") // as if the credential write already landed…
	f.setIdentity("andy@avisoma.com")
	writePendingSwitchMarker(f.home, "trecs") // …but the patch never ran.
	before := treeState(t, f.home)

	// A retry of the SAME target must refuse…
	if _, _, err := switchAccount("trecs"); err == nil {
		t.Fatal("err = nil for a retry with a pending marker, want a refusal")
	}
	// …and so must a switch to a THIRD, unrelated account — the case that
	// would otherwise corrupt trecs' real snapshot with the misattributed
	// live credential.
	if _, _, err := switchAccount("third"); err == nil {
		t.Fatal("err = nil for a different target with a pending marker, want a refusal")
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountRefusesCredentialOnlyTarget proves the precondition that
// keeps currentAccountName's plain email matching always accurate: a snapshot
// with no .account.json (e.g. one captured by the standalone claude-switch
// script's OLD manual setup step, before this tool existed, or simply never
// finished with `account save`) is refused rather than silently applied.
// Applying it anyway would leave ~/.claude.json naming the OUTGOING account
// while the credential became the incoming one — an earlier version of this
// code tried to paper over exactly that split state with a "last switch"
// marker and an independent review found it could still misattribute a LATER
// switch's backup to the wrong snapshot, corrupting it. Refusing upstream, and
// pointing at the fix (`account save`), is what actually closes the gap.
func TestSwitchAccountRefusesCredentialOnlyTarget(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "") // credential only, no .account.json
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	_, _, err := switchAccount("trecs")
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "account save trecs") {
		t.Fatalf("err = %v, want it to name the fix (`account save trecs`)", err)
	}
	// A refused switch must be a true no-op, same guarantee as the
	// already-active case: nothing on disk moved.
	assertTreeUnchanged(t, f.home, before)
	if got := f.live(); got != credBlob("tok-avisoma") {
		t.Fatalf("live credential = %q, want it untouched", got)
	}
}

// TestSwitchAccountRefusesNullIdentity proves the precondition checks for a
// USABLE identity, not merely a file that exists and parses. identitySlice
// writes an explicit JSON null for any key ~/.claude.json didn't have at
// capture time (jq parity), so a snapshot saved while not actually logged in
// is syntactically valid but practically empty — exactly the shape this test
// constructs directly, bypassing saveAccountSnapshot to make the point that
// the check is on the DATA, not on which code path produced the file.
func TestSwitchAccountRefusesNullIdentity(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	writeSnapshotFixture(t, f.home, "trecs", "tok-trecs", "")
	if err := os.WriteFile(snapshotAccountPath(f.home, "trecs"), []byte(`{"oauthAccount":null,"userID":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	if _, _, err := switchAccount("trecs"); err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountRefusesMismatchedKeyCasing proves the precondition
// validates the SAME map patchIdentityCache will actually act on, not a more
// lenient re-parse. snapshotAccountEmail (used for display purposes
// elsewhere) unmarshals into a Go struct, which falls back to
// case-insensitive key matching — "OAuthAccount" would satisfy it. But
// patchIdentityCache looks this map up by the exact key "oauthAccount", so a
// snapshot with any other casing would pass a struct-based check while
// patchIdentityCache silently found nothing to copy — reopening the split
// state the precondition exists to prevent, silently, on a switch that
// otherwise looks entirely successful.
func TestSwitchAccountRefusesMismatchedKeyCasing(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	writeSnapshotFixture(t, f.home, "trecs", "tok-trecs", "")
	if err := os.WriteFile(snapshotAccountPath(f.home, "trecs"),
		[]byte(`{"OAuthAccount":{"emailAddress":"andy@trecs.aero"},"userID":"uid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	if _, _, err := switchAccount("trecs"); err == nil {
		t.Fatal("err = nil, want a refusal (patchIdentityCache would silently apply nothing)")
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountRefusesWhenOutgoingCredentialUnreadable proves the sequencing
// guarantee: a named outgoing account whose live credential exists but cannot
// be read (permission error, locked keychain — anything other than genuinely
// not existing) is not overwritten, because doing so would discard a
// credential that might still be there with zero backup taken.
func TestSwitchAccountRefusesWhenOutgoingCredentialUnreadable(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setIdentity("andy@avisoma.com") // identity says avisoma is live…
	f.setLive("tok-avisoma-locked")
	// …but the live credential exists and is unreadable (not merely absent) —
	// chmod 000 forces a permission error distinct from os.ErrNotExist, the
	// exact distinction backupOutgoing must make.
	if err := os.Chmod(f.livePath(), 0000); err != nil {
		t.Fatal(err)
	}

	_, _, switchErr := switchAccount("trecs")

	// Restore readability before asserting — the point under test is that
	// switchAccount refused and left the file's *content* untouched, not that
	// the file stayed unreadable (which chmod alone already guarantees and
	// isn't what a compromised switch would violate anyway).
	if err := os.Chmod(f.livePath(), 0600); err != nil {
		t.Fatal(err)
	}
	if switchErr == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if got, err := os.ReadFile(f.livePath()); err != nil || string(got) != credBlob("tok-avisoma-locked") {
		t.Fatalf("live credential changed (read err = %v, got = %q), want it left alone", err, got)
	}
	outgoing, err := os.ReadFile(snapshotCredentialPath(f.home, "avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	if string(outgoing) != credBlob("tok-avisoma") {
		t.Fatalf("outgoing snapshot = %q, want it left alone", outgoing)
	}
	if _, err := os.Stat(snapshotCredentialPath(f.home, rescueSnapshotName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rescue file exists after a refused switch (stat err = %v)", err)
	}
}

// TestSwitchAccountFirstEverSwitchNoCredentialAtAll proves the OTHER side of
// the same distinction: a genuinely absent live credential (os.ErrNotExist,
// not merely unreadable) has nothing to lose, so the switch proceeds without
// a rescue copy even when the identity cache confidently (but wrongly) names
// an outgoing account.
func TestSwitchAccountFirstEverSwitchNoCredentialAtAll(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setIdentity("andy@avisoma.com") // stale/wrong identity, no live credential file at all

	if _, _, err := switchAccount("trecs"); err != nil {
		t.Fatalf("err = %v, want the switch to proceed (nothing to lose)", err)
	}
	if got := accessTokenOf(t, f.live()); got != fixtureAccessForOrig("tok-trecs") {
		t.Fatalf("live access token = %q, want the rotated blob", got)
	}
	if _, err := os.Stat(snapshotCredentialPath(f.home, rescueSnapshotName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rescue file exists when there was nothing to back up (stat err = %v)", err)
	}
}

// TestSwitchAccountFirstEverSwitch covers a machine with no live credential at
// all and no matching identity: nothing can be lost, so the switch proceeds
// without a rescue copy rather than failing.
func TestSwitchAccountFirstEverSwitch(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	if err := os.WriteFile(identityCachePath(f.home), []byte(`{"numStartups":1}`), 0600); err != nil {
		t.Fatal(err)
	}

	email, _, err := switchAccount("avisoma")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if email != "andy@avisoma.com" {
		t.Fatalf("email = %q, want andy@avisoma.com", email)
	}
	if got := accessTokenOf(t, f.live()); got != fixtureAccessForOrig("tok-avisoma") {
		t.Fatalf("live access token = %q, want the rotated blob", got)
	}
	if _, err := os.Stat(snapshotCredentialPath(f.home, rescueSnapshotName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rescue file written with nothing to rescue (stat err = %v)", err)
	}
}

// TestSaveAccountSnapshot proves the explicit capture writes both files, 0600,
// with exactly the identity keys, and that re-running it refreshes them.
func TestSaveAccountSnapshot(t *testing.T) {
	f := newAccountFixture(t)
	f.setLive("tok-live")
	f.setIdentity("andy@avisoma.com")

	if err := saveAccountSnapshot("avisoma", false); err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, path := range []string{
		snapshotCredentialPath(f.home, "avisoma"),
		snapshotAccountPath(f.home, "avisoma"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("%s mode = %v, want 0600", path, perm)
		}
	}
	if got := snapshotAccountEmail("avisoma"); got != "andy@avisoma.com" {
		t.Fatalf("snapshot email = %q", got)
	}
	identity, err := os.ReadFile(snapshotAccountPath(f.home, "avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	var slice map[string]json.RawMessage
	if err := json.Unmarshal(identity, &slice); err != nil {
		t.Fatal(err)
	}
	if len(slice) != 2 {
		t.Fatalf("identity snapshot = %s, want exactly two keys", identity)
	}
	if got := readPendingSwitchMarker(f.home); got != "" {
		t.Fatalf("pending marker = %q after a save, want none armed", got)
	}

	// Re-running after a relogin into the SAME account overwrites both files —
	// the documented refresh-after-relogin case, which the mismatch guard must
	// never get in the way of.
	f.setLive("tok-relogged")
	f.setIdentity("andy@avisoma.com")
	if err := saveAccountSnapshot("avisoma", false); err != nil {
		t.Fatalf("second save: %v", err)
	}
	cred, err := os.ReadFile(snapshotCredentialPath(f.home, "avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cred) != credBlob("tok-relogged") {
		t.Fatalf("credential snapshot = %q, want the refreshed one", cred)
	}
	if got := snapshotAccountEmail("avisoma"); got != "andy@avisoma.com" {
		t.Fatalf("snapshot email = %q, want the refreshed one", got)
	}
}

// TestSaveAccountSnapshotRefusesForeignAccount covers fix 3's second half:
// saving over a snapshot that stands for a DIFFERENT account would file this
// account's credential under the other one's name, which is the exact
// misattribution `wrong identity` reports after the fact. --force is the
// deliberate override, and it still clears the pending-switch marker, so save
// stays the one complete recovery step the marker's own message points at.
func TestSaveAccountSnapshotRefusesForeignAccount(t *testing.T) {
	f := newAccountFixture(t)
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	if err := saveAccountSnapshot("avisoma", false); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Now logged into a different account; the name still stands for the first.
	f.setLive("tok-trecs")
	f.setIdentity("andy@trecs.aero")
	writePendingSwitchMarker(f.home, "avisoma")

	err := saveAccountSnapshot("avisoma", false)
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	for _, want := range []string{"andy@avisoma.com", "andy@trecs.aero", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to name %q", err, want)
		}
	}
	if cred := f.snapshotCred("avisoma"); cred != credBlob("tok-avisoma") {
		t.Fatalf("snapshot credential = %q, want the refused save to have touched nothing", cred)
	}
	if got := readPendingSwitchMarker(f.home); got != "avisoma" {
		t.Fatalf("pending marker = %q, want a refused save to leave it armed", got)
	}

	if err := saveAccountSnapshot("avisoma", true); err != nil {
		t.Fatalf("--force save: %v", err)
	}
	if cred := f.snapshotCred("avisoma"); cred != credBlob("tok-trecs") {
		t.Fatalf("snapshot credential = %q, want --force to have reassigned it", cred)
	}
	if got := snapshotAccountEmail("avisoma"); got != "andy@trecs.aero" {
		t.Fatalf("snapshot email = %q, want the reassigned one", got)
	}
	if got := readPendingSwitchMarker(f.home); got != "" {
		t.Fatalf("pending marker = %q, want --force to clear it like any other save", got)
	}
}

// TestSaveAccountSnapshotFirstSaveIsNeverRefused proves the mismatch guard only
// fires when there is something to disagree with: a name nobody has claimed yet
// saves exactly as it always did.
func TestSaveAccountSnapshotFirstSaveIsNeverRefused(t *testing.T) {
	f := newAccountFixture(t)
	f.setLive("tok-live")
	f.setIdentity("andy@trecs.aero")
	if err := saveAccountSnapshot("brand-new", false); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := snapshotAccountEmail("brand-new"); got != "andy@trecs.aero" {
		t.Fatalf("snapshot email = %q", got)
	}
}

// TestSaveAccountSnapshotClearsPendingMarker proves `account save` is a
// complete, self-contained recovery step from an interrupted switch: it
// doesn't just resync the named snapshot, it also disarms the pending-switch
// marker, since capturing what's live right now under a name is exactly the
// human confirmation the marker exists to wait for.
func TestSaveAccountSnapshotClearsPendingMarker(t *testing.T) {
	f := newAccountFixture(t)
	f.setLive("tok-live")
	f.setIdentity("andy@avisoma.com")
	writePendingSwitchMarker(f.home, "avisoma")

	if err := saveAccountSnapshot("avisoma", false); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := readPendingSwitchMarker(f.home); got != "" {
		t.Fatalf("pending marker = %q after save, want it cleared", got)
	}
}

// TestSaveAccountSnapshotRejectsBadNames proves a snapshot name never reaches
// the filesystem unvalidated — it becomes a file-name segment.
func TestSaveAccountSnapshotRejectsBadNames(t *testing.T) {
	f := newAccountFixture(t)
	f.setLive("tok-live")
	f.setIdentity("andy@avisoma.com")

	for _, name := range []string{"", "../escape", "has/slash", "dot.name", rescueSnapshotName} {
		if err := saveAccountSnapshot(name, false); err == nil {
			t.Fatalf("saveAccountSnapshot(%q) = nil, want an error", name)
		}
	}
}

func TestAccountNameOK(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"avisoma", true},
		{"trecs", true},
		{"work-2", true},
		{"under_score", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"has.dot", false},
		{"space name", false},
		{rescueSnapshotName, false},
		{strings.Repeat("a", 65), false},
	}
	for _, tt := range tests {
		if got := accountNameOK(tt.name); got != tt.want {
			t.Errorf("accountNameOK(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCurrentAccountName(t *testing.T) {
	tests := []struct {
		name      string
		liveEmail string
		want      string
	}{
		{"matches a snapshot", "andy@avisoma.com", "avisoma"},
		{"matches case-insensitively", "ANDY@Trecs.Aero", "trecs"},
		{"matches nothing", "someone@elsewhere.example", ""},
		{"live email unknown", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAccountFixture(t)
			f.snapshot("avisoma", "tok-a", "andy@avisoma.com")
			f.snapshot("trecs", "tok-t", "andy@trecs.aero")
			if tt.liveEmail == "" {
				if err := os.WriteFile(identityCachePath(f.home), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				f.setIdentity(tt.liveEmail)
			}
			if got := currentAccountName(); got != tt.want {
				t.Fatalf("currentAccountName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIdentitySnapshotEmail(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"normal", `{"oauthAccount":{"emailAddress":"andy@avisoma.com"}}`, "andy@avisoma.com"},
		{"missing key", `{"userID":"uid"}`, ""},
		{"explicit null", `{"oauthAccount":null}`, ""},
		{"mismatched key casing", `{"OAuthAccount":{"emailAddress":"andy@avisoma.com"}}`, ""},
		{"empty email", `{"oauthAccount":{"emailAddress":""}}`, ""},
		{"non-object value", `{"oauthAccount":"not an object"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var snapshot map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.json), &snapshot); err != nil {
				t.Fatal(err)
			}
			if got := identitySnapshotEmail(snapshot); got != tt.want {
				t.Fatalf("identitySnapshotEmail(%s) = %q, want %q", tt.json, got, tt.want)
			}
		})
	}
}

func TestIdentitySlice(t *testing.T) {
	tests := []struct {
		name  string
		cache string
		want  string
	}{
		{
			name:  "both keys present",
			cache: `{"oauthAccount":{"emailAddress":"a@b.c"},"userID":"uid","other":1}`,
			want:  `{"oauthAccount":{"emailAddress":"a@b.c"},"userID":"uid"}`,
		},
		{
			name:  "absent key becomes null, matching jq's slice",
			cache: `{"oauthAccount":{"emailAddress":"a@b.c"}}`,
			want:  `{"oauthAccount":{"emailAddress":"a@b.c"},"userID":null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := identitySlice([]byte(tt.cache))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			// Compared as decoded values: the slice is written indented (jq's own
			// shape), which a byte comparison would trip over.
			var gotValue, wantValue any
			if err := json.Unmarshal(got, &gotValue); err != nil {
				t.Fatalf("slice is not valid JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantValue); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Fatalf("slice = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestPatchIdentityCache proves the merge is surgical: the identity keys are
// replaced, an explicit null in the snapshot is ignored rather than copied over
// a good value, and every other key survives untouched.
func TestPatchIdentityCache(t *testing.T) {
	f := newAccountFixture(t)
	f.setIdentity("andy@avisoma.com")

	snapshot := map[string]json.RawMessage{
		"oauthAccount": json.RawMessage(`{"emailAddress":"andy@trecs.aero"}`),
		"userID":       json.RawMessage(`null`),
		"ignored":      json.RawMessage(`"nope"`),
	}
	if err := patchIdentityCache(snapshot); err != nil {
		t.Fatalf("err = %v", err)
	}
	cache := readIdentityCache(t, f.home)
	if got := loadAccountEmail(); got != "andy@trecs.aero" {
		t.Fatalf("email = %q, want the patched one", got)
	}
	if got := string(cache["userID"]); got != `"uid-andy@avisoma.com"` {
		t.Fatalf("userID = %s, want the existing value kept (snapshot held null)", got)
	}
	if _, ok := cache["ignored"]; ok {
		t.Fatal("patch copied a key outside oauthAccount/userID")
	}
	if _, ok := cache["projects"]; !ok {
		t.Fatalf("patch dropped an unrelated key: %v", keysOf(cache))
	}
}

// TestPatchIdentityCachePreservesMode proves the temp-file-then-rename write
// does not silently retighten a file Claude Code owns.
func TestPatchIdentityCachePreservesMode(t *testing.T) {
	f := newAccountFixture(t)
	f.setIdentity("andy@avisoma.com")
	path := identityCachePath(f.home)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	err := patchIdentityCache(map[string]json.RawMessage{
		"oauthAccount": json.RawMessage(`{"emailAddress":"andy@trecs.aero"}`),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Fatalf("mode = %v, want 0644 preserved", perm)
	}
}

// TestCredentialFileRoundTrip exercises the non-darwin legs directly, so both
// platform branches are covered from either host. The write is atomic, so no
// temp file may survive it.
func TestCredentialFileRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := writeCredentialFile([]byte(credBlob("tok"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readCredentialFile()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != credBlob("tok") {
		t.Fatalf("read = %q, want %q", got, credBlob("tok"))
	}
	info, err := os.Stat(liveCredentialPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("mode = %v, want 0600", perm)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("~/.claude holds %d entries, want just the credential file", len(entries))
	}
}

// TestWithAccountLockSerializes proves the advisory lock is real: overlapping
// callers never run the critical section at the same time. The lock file is
// opened per call precisely so this holds within one process — flock locks are
// per open file description, and a cached handle would let both callers in.
func TestWithAccountLockSerializes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var inFlight, maxInFlight atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := withAccountLock(func() error {
				n := inFlight.Add(1)
				for {
					max := maxInFlight.Load()
					if n <= max || maxInFlight.CompareAndSwap(max, n) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				inFlight.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("withAccountLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent holders = %d, want 1", got)
	}
}

// TestSwitchAccountConcurrent proves two overlapping switches serialize rather
// than interleave: whichever runs second observes the first's fully-applied
// state, so the end state is one account applied completely — never a live
// credential from one account with an identity cache from another.
func TestSwitchAccountConcurrent(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshot("third", "tok-third", "andy@third.example")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"trecs", "third"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			_, _, errs[i] = switchAccount(name)
		}(i, name)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("switch %d: %v", i, err)
		}
	}

	liveTok := accessTokenOf(t, f.live())
	email := loadAccountEmail()
	switch {
	case liveTok == fixtureAccessForOrig("tok-trecs") && email == "andy@trecs.aero":
	case liveTok == fixtureAccessForOrig("tok-third") && email == "andy@third.example":
	default:
		t.Fatalf("torn state: live access = %q identity = %q", liveTok, email)
	}
	// The account both switches started from kept a copy of its own credential.
	outgoing, err := os.ReadFile(snapshotCredentialPath(f.home, "avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	if string(outgoing) != credBlob("tok-avisoma") {
		t.Fatalf("avisoma snapshot = %q, want its credential synced back", outgoing)
	}
}

// readIdentityCache decodes ~/.claude.json into its raw keys.
func readIdentityCache(t *testing.T, home string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(identityCachePath(home))
	if err != nil {
		t.Fatal(err)
	}
	var cache map[string]json.RawMessage
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatal(err)
	}
	return cache
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// setSnapshotCred replaces one snapshot's credential blob with arbitrary bytes,
// so the validation tests can build a credential the ordinary fixture never
// produces (no refresh token, an expired one, a still-valid access token).
func (f *accountFixture) setSnapshotCred(name, blob string) {
	f.t.Helper()
	if err := os.WriteFile(snapshotCredentialPath(f.home, name), []byte(blob), 0600); err != nil {
		f.t.Fatal(err)
	}
}

// stubSwitchSessions installs a fake session list for switchSessionWarnings and
// restores the real collector afterwards.
func stubSwitchSessions(t *testing.T, sessions []Session, err error) {
	t.Helper()
	prev := collectSessionsForSwitch
	collectSessionsForSwitch = func() ([]Session, error) { return sessions, err }
	t.Cleanup(func() { collectSessionsForSwitch = prev })
}

// stubProfileEmail installs a fake identity probe. TestMain defaults the seam to
// a panic, so every test that can reach the probe has to say what it answers.
// The token→email cache is reset on both sides of the swap: it is a package-level
// singleton that outlives any one test, so without this one test's stubbed
// identity would be served to the next.
func stubProfileEmail(t *testing.T, fn func(token string) (string, error)) {
	t.Helper()
	prev := profileEmailFetch
	profileEmailFetch = fn
	profileEmails.reset()
	t.Cleanup(func() {
		profileEmailFetch = prev
		profileEmails.reset()
	})
}

// TestSwitchAccountRefusesExpiredRefreshToken is fix 3's core case. A snapshot
// whose refresh token has expired is not a login any more: Claude Code's next
// refresh is answered with invalid_grant and it zeroes the stored credential, so
// installing one is a switch that logs the host out. It is refused before
// anything at all is written, and the message names the one command that fixes
// it.
func TestSwitchAccountRefusesExpiredRefreshToken(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setSnapshotCred("trecs", credBlobAt("tok-trecs", time.Time{}, time.Now().Add(-time.Hour)))
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	_, _, err := switchAccount("trecs")
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "account save trecs") {
		t.Fatalf("err = %v, want it to name the fix", err)
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountRefusesCredentialWithoutRefreshToken covers the same hazard
// reached the other way: a blob that never had a refresh token at all.
func TestSwitchAccountRefusesCredentialWithoutRefreshToken(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setSnapshotCred("trecs", `{"claudeAiOauth":{"accessToken":"tok-trecs"}}`)
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	_, _, err := switchAccount("trecs")
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("err = %v, want it to name the missing refresh token", err)
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountAcceptsExpiredAccessToken proves the validation refuses only
// what actually breaks: an access token past its expiry is the normal state of a
// parked snapshot, and refreshing it is exactly what the refresh token is for.
// It is also the case that must NOT probe the profile endpoint — the token could
// not authenticate it anyway — which the panicking default seam enforces.
func TestSwitchAccountAcceptsExpiredAccessToken(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setSnapshotCred("trecs", credBlobAt("tok-trecs", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)))
	f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-trecs")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")

	email, _, err := switchAccount("trecs")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if email != "andy@trecs.aero" {
		t.Fatalf("email = %q", email)
	}
}

// TestSwitchAccountRefusesMisattributedSnapshot is the network half of fix 3:
// when the snapshot's access token is still valid, the profile endpoint can say
// whose it really is, and a disagreement with the identity file means switching
// would leave this host logged in as one account and labelled as the other —
// the same `wrong identity` the usage poller reports after the fact, caught
// before it becomes the live credential.
func TestSwitchAccountRefusesMisattributedSnapshot(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setSnapshotCred("trecs", credBlobAt("tok-trecs", time.Now().Add(time.Hour), time.Now().Add(24*time.Hour)))
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	stubProfileEmail(t, func(token string) (string, error) {
		if token != "tok-trecs" {
			t.Fatalf("probed with %q, want the snapshot's own access token", token)
		}
		return "someone@elsewhere.example", nil
	})
	before := treeState(t, f.home)

	_, _, err := switchAccount("trecs")
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	for _, want := range []string{"someone@elsewhere.example", "andy@trecs.aero", "account save trecs"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to name %q", err, want)
		}
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountProbeFailureIsNotBlocking proves the identity probe is
// advisory. Validation has to work on a laptop with no network, so an
// unreachable or throttled endpoint leaves the two offline checks — the ones
// that matter — as the whole decision.
func TestSwitchAccountProbeFailureIsNotBlocking(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.setSnapshotCred("trecs", credBlobAt("tok-trecs", time.Now().Add(time.Hour), time.Now().Add(24*time.Hour)))
	f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-trecs")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	stubProfileEmail(t, func(string) (string, error) {
		return "", &usageHTTPError{Status: 429}
	})

	email, _, err := switchAccount("trecs")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if email != "andy@trecs.aero" {
		t.Fatalf("email = %q", email)
	}
}

// TestSwitchAccountWarnsAboutRunningSessions is fix 1. Sessions of the outgoing
// account hold its token and rewrite the live credential when they refresh, so a
// switch says so — and proceeds regardless, because the command is quite likely
// being typed inside one of those very sessions.
func TestSwitchAccountWarnsAboutRunningSessions(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-trecs")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	stubSwitchSessions(t, []Session{{PID: 4242}, {PID: 17}}, nil)

	email, warnings, err := switchAccount("trecs")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if email != "andy@trecs.aero" {
		t.Fatalf("email = %q, want the switch to have gone through anyway", email)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	// Pids are sorted, so the same set always renders the same line.
	for _, want := range []string{"2 Claude Code sessions", "pid 17, 4242", "account switch trecs"} {
		if !strings.Contains(warnings[0], want) {
			t.Fatalf("warning = %q, want it to contain %q", warnings[0], want)
		}
	}
	if got := accessTokenOf(t, f.live()); got != fixtureAccessForOrig("tok-trecs") {
		t.Fatalf("live access token = %q, want the rotated blob", got)
	}
}

// TestSwitchAccountNoWarningWithoutSessions covers the two quiet cases: nothing
// running, and a collector that failed. Being unable to enumerate sessions is
// not a reason to invent a warning — or to fail a switch.
func TestSwitchAccountNoWarningWithoutSessions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sessions []Session
		err      error
	}{
		{name: "no sessions", sessions: nil},
		{name: "collector failed", err: errors.New("read process tree: boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAccountFixture(t)
			f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
			f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-trecs")
			f.setLive("tok-avisoma")
			f.setIdentity("andy@avisoma.com")
			stubSwitchSessions(t, tc.sessions, tc.err)

			_, warnings, err := switchAccount("trecs")
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(warnings) != 0 {
				t.Fatalf("warnings = %v, want none", warnings)
			}
		})
	}
}

// TestSwitchAccountAlreadyActiveRaisesNoWarning pins the placement of the check:
// a no-op switch touches nothing, so there is no hazard to warn about — and the
// session collector is never even consulted.
func TestSwitchAccountAlreadyActiveRaisesNoWarning(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.setLive("tok-avisoma-live")
	f.setIdentity("andy@avisoma.com")
	stubSwitchSessions(t, nil, errors.New("collector must not be called for a no-op"))

	_, warnings, err := switchAccount("avisoma")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// TestRunningSessionsWarningPhrasing covers singular/plural agreement and the
// pid-list cap, which only matters on a busy host but is what keeps the line
// readable there.
func TestRunningSessionsWarningPhrasing(t *testing.T) {
	one := runningSessionsWarning("trecs", []Session{{PID: 9}})
	if !strings.Contains(one, "1 Claude Code session is still running (pid 9)") {
		t.Fatalf("singular = %q", one)
	}
	if !strings.Contains(one, "It holds") {
		t.Fatalf("singular verb missing: %q", one)
	}

	many := make([]Session, 0, switchWarningPIDLimit+3)
	for i := switchWarningPIDLimit + 3; i > 0; i-- {
		many = append(many, Session{PID: i})
	}
	line := runningSessionsWarning("trecs", many)
	if !strings.Contains(line, "and 3 more") {
		t.Fatalf("capped list missing: %q", line)
	}
	if strings.Contains(line, "11,") {
		t.Fatalf("pid past the cap was listed: %q", line)
	}
}

// TestRemoveAccountSnapshot is fix 4's happy path: every file the snapshot owns
// goes, and nothing else is touched — not the live credential, not
// ~/.claude.json, not another account's snapshot.
func TestRemoveAccountSnapshot(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")

	plan, err := planAccountRemoval("trecs")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Live {
		t.Fatal("plan.Live = true for a snapshot that is not the live account")
	}
	removed, wasLive, err := removeAccountSnapshot("trecs")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if wasLive {
		t.Fatal("wasLive = true for a snapshot that is not the live account")
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want the credential and the identity file", removed)
	}
	names, _ := snapshotAccountNames()
	if containsAccountName(names, "trecs") {
		t.Fatalf("names = %v, want trecs gone", names)
	}
	if !containsAccountName(names, "avisoma") {
		t.Fatalf("names = %v, want avisoma untouched", names)
	}
	if got := f.live(); got != credBlob("tok-avisoma") {
		t.Fatalf("live credential = %q, want it untouched", got)
	}
	if got := loadAccountEmail(); got != "andy@avisoma.com" {
		t.Fatalf("live email = %q, want it untouched", got)
	}
}

// TestRemoveAccountSnapshotBothPlatformCredentials proves a machine that has
// held both file shapes loses both — leaving one behind would keep the account
// listed by snapshotAccountNames on the other platform.
func TestRemoveAccountSnapshotBothPlatformCredentials(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	other := ".trecs.keychain-cred"
	if runtime.GOOS == "darwin" {
		other = ".trecs.credentials.json"
	}
	otherPath := filepath.Join(f.home, ".claude", other)
	if err := os.WriteFile(otherPath, []byte(credBlob("tok-legacy")), 0600); err != nil {
		t.Fatal(err)
	}
	f.setLive("tok-live")
	f.setIdentity("andy@avisoma.com")

	removed, _, err := removeAccountSnapshot("trecs")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed = %v, want all three files", removed)
	}
	if _, err := os.Stat(otherPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s = %v, want it gone", otherPath, err)
	}
}

// TestRemoveAccountSnapshotLiveAccountIsFlagged proves the plan tells a caller
// when the removal costs it the ability to switch back. The removal itself is
// still allowed — it deletes a parked copy, not the login.
func TestRemoveAccountSnapshotLiveAccountIsFlagged(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.setLive("tok-avisoma-live")
	f.setIdentity("andy@avisoma.com")

	plan, err := planAccountRemoval("avisoma")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Live {
		t.Fatal("plan.Live = false for the snapshot standing for the live account")
	}
	_, wasLive, err := removeAccountSnapshot("avisoma")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !wasLive {
		t.Fatal("wasLive = false; the removal has to report what it actually removed")
	}
	if got := f.live(); got != credBlob("tok-avisoma-live") {
		t.Fatalf("live credential = %q, want removing a snapshot to leave the login alone", got)
	}
	if got := loadAccountEmail(); got != "andy@avisoma.com" {
		t.Fatalf("live email = %q, want it untouched", got)
	}
}

// TestRemoveAccountSnapshotUnknownName proves an unrecognized name reaches no
// unlink at all — and that the rescue slot is therefore unremovable, since
// snapshotAccountNames filters it out of the only listing this consults.
func TestRemoveAccountSnapshotUnknownName(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	if err := os.WriteFile(snapshotCredentialPath(f.home, rescueSnapshotName),
		[]byte(credBlob("tok-rescue")), 0600); err != nil {
		t.Fatal(err)
	}
	f.setLive("tok-live")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	for _, name := range []string{"nope", rescueSnapshotName, "../escape"} {
		if _, err := planAccountRemoval(name); !errors.Is(err, errUnknownAccount) {
			t.Fatalf("planAccountRemoval(%q) = %v, want errUnknownAccount", name, err)
		}
		if _, _, err := removeAccountSnapshot(name); !errors.Is(err, errUnknownAccount) {
			t.Fatalf("removeAccountSnapshot(%q) = %v, want errUnknownAccount", name, err)
		}
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountNoOpSkipsValidation pins step 2.4's placement. Switching to
// the account already live installs nothing, so there is nothing to validate —
// and validating anyway would turn a guaranteed no-op into a refusal whenever
// the parked snapshot had gone stale, which is precisely when someone reaches
// for a switch. The panicking profile seam also proves no probe was spent on it.
func TestSwitchAccountNoOpSkipsValidation(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	// A snapshot that would be refused outright if it were ever installed.
	f.setSnapshotCred("avisoma", credBlobAt("tok-avisoma", time.Now().Add(time.Hour), time.Now().Add(-time.Hour)))
	f.setLive("tok-avisoma-live")
	f.setIdentity("andy@avisoma.com")
	before := treeState(t, f.home)

	email, warnings, err := switchAccount("avisoma")
	if err != nil {
		t.Fatalf("err = %v, want a no-op, not a refusal", err)
	}
	if email != "andy@avisoma.com" {
		t.Fatalf("email = %q", email)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for a no-op", warnings)
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountRotatesSnapshotAndLive is the happy path for parked-snapshot
// rotation: a canned token response is written to BOTH the snapshot file and
// the live credential, so Claude Code starts with a fresh access token.
func TestSwitchAccountRotatesSnapshotAndLive(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-trecs")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")

	if _, _, err := switchAccount("trecs"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := accessTokenOf(t, f.live()); got != fixtureAccessForOrig("tok-trecs") {
		t.Fatalf("live access = %q, want %q", got, fixtureAccessForOrig("tok-trecs"))
	}
	if got := accessTokenOf(t, f.snapshotCred("trecs")); got != fixtureAccessForOrig("tok-trecs") {
		t.Fatalf("snapshot access = %q, want %q", got, fixtureAccessForOrig("tok-trecs"))
	}
}

// TestSwitchAccountRefreshNetworkErrorInstallsOriginal: a transient rotate
// failure must not block a switch. The original snapshot bytes are installed
// live, matching today's expired-access-still-installs behaviour.
func TestSwitchAccountRefreshNetworkErrorInstallsOriginal(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-trecs")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		return nil, errors.New("dial tcp: no route to host")
	})

	if _, _, err := switchAccount("trecs"); err != nil {
		t.Fatalf("err = %v, want the switch to succeed with the original snapshot", err)
	}
	if got := f.live(); got != credBlob("tok-trecs") {
		t.Fatalf("live credential = %q, want the original snapshot bytes", got)
	}
	if got := f.snapshotCred("trecs"); got != credBlob("tok-trecs") {
		t.Fatalf("snapshot = %q, want it left as the original bytes", got)
	}
}

// TestSwitchAccountRefreshInvalidGrantRefuses: a dead refresh token is a
// refusal, not an install. Nothing is written — not live, not the snapshot.
func TestSwitchAccountRefreshInvalidGrantRefuses(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-trecs", "andy@trecs.aero")
	f.snapshotIdentity("trecs", "andy@trecs.aero", "uid-trecs")
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		return nil, &oauthRefreshError{Status: 400, Code: "invalid_grant"}
	})
	before := treeState(t, f.home)

	_, _, err := switchAccount("trecs")
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "account save trecs") {
		t.Fatalf("err = %v, want it to name the fix", err)
	}
	assertTreeUnchanged(t, f.home, before)
}

// TestSwitchAccountNoOpDoesNotRefresh: a switch to the already-live account
// returns before validate AND before rotate. Refreshing a parked copy of the
// live account would race Claude Code's own rotation of the live grant.
func TestSwitchAccountNoOpDoesNotRefresh(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("avisoma", "tok-avisoma", "andy@avisoma.com")
	f.setLive("tok-avisoma-live")
	f.setIdentity("andy@avisoma.com")
	calls := 0
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		calls++
		return fixtureOAuthRefresh("", nil)
	})

	if _, _, err := switchAccount("avisoma"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 0 {
		t.Fatalf("oauthTokenRefresh calls = %d, want 0 for a no-op switch", calls)
	}
}
