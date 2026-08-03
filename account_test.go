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
	cuFetchFunc = func(context.Context, string) ([]byte, error) { panic("test reached the real cu CLI") }
	claudeSummarizeFunc = func(context.Context, string, []byte) ([]byte, error) { panic("test reached the real claude CLI") }
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
	return f
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
func credBlob(token string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, token)
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

	email, err := switchAccount("nope")
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

	email, err := switchAccount("avisoma")
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

	if _, err := switchAccount("avisoma"); err != nil {
		t.Fatalf("err = %v", err)
	}
	rescue, err := os.ReadFile(snapshotCredentialPath(f.home, rescueSnapshotName))
	if err != nil {
		t.Fatalf("rescue backup missing: %v", err)
	}
	if string(rescue) != credBlob("tok-stranger") {
		t.Fatalf("rescue = %q, want the outgoing live credential", rescue)
	}
	if got := f.live(); got != credBlob("tok-avisoma") {
		t.Fatalf("live credential = %q, want avisoma's snapshot", got)
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

	email, err := switchAccount("trecs")
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

	// Step 5: the live credential is now the target's.
	if got := f.live(); got != credBlob("tok-trecs") {
		t.Fatalf("live credential = %q, want trecs' snapshot", got)
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
	if _, err := switchAccount("trecs"); err == nil {
		t.Fatal("err = nil for a retry with a pending marker, want a refusal")
	}
	// …and so must a switch to a THIRD, unrelated account — the case that
	// would otherwise corrupt trecs' real snapshot with the misattributed
	// live credential.
	if _, err := switchAccount("third"); err == nil {
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

	_, err := switchAccount("trecs")
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

	if _, err := switchAccount("trecs"); err == nil {
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

	if _, err := switchAccount("trecs"); err == nil {
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

	_, switchErr := switchAccount("trecs")

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

	if _, err := switchAccount("trecs"); err != nil {
		t.Fatalf("err = %v, want the switch to proceed (nothing to lose)", err)
	}
	if got := f.live(); got != credBlob("tok-trecs") {
		t.Fatalf("live credential = %q, want trecs' snapshot", got)
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

	email, err := switchAccount("avisoma")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if email != "andy@avisoma.com" {
		t.Fatalf("email = %q, want andy@avisoma.com", email)
	}
	if got := f.live(); got != credBlob("tok-avisoma") {
		t.Fatalf("live credential = %q, want avisoma's snapshot", got)
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

	if err := saveAccountSnapshot("avisoma"); err != nil {
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

	// Re-running after a relogin overwrites both files.
	f.setLive("tok-relogged")
	f.setIdentity("andy@trecs.aero")
	if err := saveAccountSnapshot("avisoma"); err != nil {
		t.Fatalf("second save: %v", err)
	}
	cred, err := os.ReadFile(snapshotCredentialPath(f.home, "avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cred) != credBlob("tok-relogged") {
		t.Fatalf("credential snapshot = %q, want the refreshed one", cred)
	}
	if got := snapshotAccountEmail("avisoma"); got != "andy@trecs.aero" {
		t.Fatalf("snapshot email = %q, want the refreshed one", got)
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

	if err := saveAccountSnapshot("avisoma"); err != nil {
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
		if err := saveAccountSnapshot(name); err == nil {
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
			_, errs[i] = switchAccount(name)
		}(i, name)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("switch %d: %v", i, err)
		}
	}

	live := f.live()
	email := loadAccountEmail()
	switch live {
	case credBlob("tok-trecs"):
		if email != "andy@trecs.aero" {
			t.Fatalf("torn state: live credential is trecs' but the identity says %q", email)
		}
	case credBlob("tok-third"):
		if email != "andy@third.example" {
			t.Fatalf("torn state: live credential is third's but the identity says %q", email)
		}
	default:
		t.Fatalf("live credential = %q, want one of the two targets", live)
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
