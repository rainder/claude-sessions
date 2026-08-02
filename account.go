package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// Account switching: the Go port of the standalone claude-switch shell scripts
// (macOS Keychain-based, Linux file-based). Both tools stay interchangeable —
// identical file names, formats and locations — so either can switch an account
// on any machine with no migration step:
//
//	~/.claude/.<name>.keychain-cred    credential snapshot (macOS)
//	~/.claude/.<name>.credentials.json credential snapshot (elsewhere)
//	~/.claude/.<name>.account.json     {oauthAccount, userID} identity snapshot
//	~/.claude.json                     Claude Code's live identity cache
//
// Everything here is deliberately small and file-shaped: no OAuth refresh, no
// new dependency (the advisory lock is golang.org/x/sys/unix, already imported
// by remote.go), and no jq — the identity-cache patch is plain encoding/json.

// keychainService is the macOS Keychain generic-password service Claude Code
// stores its credential blob under, and the one claude-switch reads and writes.
// Shared with loadOAuthToken's read in usage.go by value, not by symbol, because
// that read predates this file; they must stay identical.
const keychainService = "Claude Code-credentials"

// rescueSnapshotName is the reserved single rolling slot switchAccount parks the
// live credential in before it overwrites it — see step 3 of switchAccount. It
// is a backup, not an account: it has no identity snapshot, nothing is ever
// logged into it, and switching *to* it would be meaningless. Because it lives
// beside the real snapshots under the same file name shape,
// snapshotAccountNames excludes it by this name so it never surfaces as an
// account in the usage poller, `account list`, or the Ctrl+W picker. The name
// matches the one claude-switch already writes, so both tools share the slot.
const rescueSnapshotName = "last-switch-rescue"

// accountLockFile is the advisory lock every switch and save is taken under.
// See withAccountLock.
const accountLockFile = ".account-switch.lock"

// identityCacheKeys are the only keys a switch copies out of a snapshot into
// ~/.claude.json. Everything else in that file belongs to Claude Code and is
// preserved untouched (see patchIdentityCache).
var identityCacheKeys = []string{"oauthAccount", "userID"}

// errUnknownAccount is returned (wrapped, with the known names listed) when a
// caller names a snapshot this host does not hold. The HTTP endpoint maps it to
// 400 unknown_account; the CLI prints it and exits 1.
var errUnknownAccount = errors.New("no account snapshot")

// keychainRead / keychainWrite are the macOS Keychain legs of
// readActiveCredential / writeActiveCredential, isolated behind package-level
// function variables for exactly one reason: a test must never be able to reach
// the real login Keychain, where a stray write would destroy this machine's live
// Claude Code credential. Tests replace both with a tempdir-backed fake (and
// TestMain defaults them to a panic, so forgetting the override fails closed).
// The two real implementations below are therefore deliberately not exercised by
// any test.
var (
	keychainRead  = securityFindPassword
	keychainWrite = securityAddPassword
)

// securityNotFoundExitCode is what `security find-generic-password` exits with
// when the item genuinely does not exist — verified directly against the
// installed `security` binary, not assumed. It is NOT a reliable signal on its
// own: macOS exit codes are the low byte of a Security framework OSStatus, and
// other statuses (e.g. errSecInvalidAttributeAccessCredentials = -67796) reduce
// to the same 44 mod 256. securityFindPassword therefore also requires the
// stderr text before treating this as "not found" — see there.
const securityNotFoundExitCode = 44

// securityNotFoundStderrHint is the distinctive fragment of the real "item not
// found" message ("security: SecKeychainSearchCopyNext: The specified item
// could not be found in the keychain."), checked alongside the exit code so a
// same-numbered but different failure isn't misclassified as safe-to-skip.
const securityNotFoundStderrHint = "could not be found"

// securityFindPassword prints the live credential blob via the `security` CLI,
// the same invocation loadOAuthToken and claude-switch use. No cgo, no Keychain
// bindings. A genuine "no such item" is wrapped so callers can test it with
// errors.Is(err, os.ErrNotExist), the same signal readCredentialFile's plain
// os.ReadFile already gives on the non-darwin path — one check works on both.
//
// Classifying this wrong has asymmetric cost: refusing a switch that could
// have safely proceeded is just an extra command to re-run; treating a real
// failure as "nothing to lose" would overwrite a live credential with zero
// backup. So both signals — exit code AND stderr text — must agree before this
// returns ErrNotExist; anything else (including an ambiguous exit-44 with
// different stderr) is reported as a plain, refusing error.
func securityFindPassword() ([]byte, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", keychainService, "-w")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok &&
			exitErr.ExitCode() == securityNotFoundExitCode &&
			strings.Contains(stderr.String(), securityNotFoundStderrHint) {
			return nil, fmt.Errorf("keychain read: %w", os.ErrNotExist)
		}
		return nil, fmt.Errorf("keychain read: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// securityAddPassword replaces the live credential item (-U updates in place).
// The account (-a) mirrors claude-switch exactly: $CLAUDE_SWITCH_ACCT when set,
// otherwise the current user's name — a mismatch there would leave a second item
// beside the one Claude Code reads.
//
// The blob travels on the command line, briefly visible in `ps`, because that is
// what `security` accepts and what the shell script already does; matching it is
// what keeps the two tools interchangeable.
func securityAddPassword(data []byte) error {
	acct := os.Getenv("CLAUDE_SWITCH_ACCT")
	if acct == "" {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("keychain write: resolve account name: %w", err)
		}
		acct = u.Username
	}
	// TrimSpace matches the shell's `-w "$(cat "$file")"`, which drops the
	// trailing newline `security -w` printed when the snapshot was captured.
	cmd := exec.Command("security", "add-generic-password", "-U",
		"-a", acct, "-s", keychainService, "-w", strings.TrimSpace(string(data)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain write: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// liveCredentialPath is the non-darwin live credential file — the one
// loadOAuthToken reads. On darwin there is no such file; the live credential is
// the Keychain item.
func liveCredentialPath(home string) string {
	return filepath.Join(home, ".claude", ".credentials.json")
}

// identityCachePath is Claude Code's top-level identity cache. Note this is
// $HOME/.claude.json, NOT ~/.claude/.claude.json (same trap loadAccountEmail
// documents).
func identityCachePath(home string) string {
	return filepath.Join(home, ".claude.json")
}

// readActiveCredential returns the credential blob for the account currently
// logged in on this host. The platform split mirrors loadOAuthToken's exactly:
// Keychain on darwin, ~/.claude/.credentials.json everywhere else. A snapshot
// file is never consulted here — this is the live copy, by definition.
func readActiveCredential() ([]byte, error) {
	if runtime.GOOS == "darwin" {
		return keychainRead()
	}
	return readCredentialFile()
}

// writeActiveCredential installs data as the live credential. Same platform
// split as readActiveCredential; this is the one call that actually performs a
// switch, and every backup step runs strictly before it.
func writeActiveCredential(data []byte) error {
	if runtime.GOOS == "darwin" {
		return keychainWrite(data)
	}
	return writeCredentialFile(data)
}

// readCredentialFile is readActiveCredential's non-darwin leg, split out so both
// platform legs are testable from either host.
func readCredentialFile() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(liveCredentialPath(home))
}

// writeCredentialFile is writeActiveCredential's non-darwin leg. The write is
// atomic (temp file + rename in the same directory) so a Claude Code process
// reading the file concurrently never sees a half-written credential.
func writeCredentialFile(data []byte) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return writeFileAtomic(liveCredentialPath(home), data, 0600)
}

// writeFileAtomic writes data to path through a temp file in the same directory
// followed by a rename — the crash-safety pattern SessionStore.save (state.go)
// already uses, and for the same reason: the rename is same-filesystem and
// atomic, so no reader ever observes a partial file. perm is applied explicitly
// because os.CreateTemp always creates 0600, which would silently tighten the
// mode of a file this tool only patches (~/.claude.json).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claude-sessions-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// accountNameOK reports whether name is usable as a snapshot name. Snapshot
// names become file-name segments, so the character set is deliberately narrow —
// switchAccount can only ever name a snapshot the directory listing already
// produced, but saveAccountSnapshot takes the name straight from a user, and a
// "../…" there would write outside ~/.claude entirely.
func accountNameOK(name string) bool {
	if name == "" || len(name) > 64 || name == rescueSnapshotName {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// currentAccountName is the snapshot name standing for the account logged in on
// this host: the one whose .<name>.account.json email matches the live
// ~/.claude.json email. "" when the live email is unknown or no snapshot claims
// it — a first-ever switch, a renamed account, or a partially-applied earlier
// switch. That "" case is exactly why switchAccount's rescue backup is
// unconditional.
//
// Matching is by email (emailMatchesLive), the same comparison the known-account
// poller uses, so both agree on which snapshot is "current". This is only
// reliable because switchAccount refuses to switch TO a snapshot with no
// identity file (see switchAccountLocked step 6) — every switch this tool ever
// performs leaves ~/.claude.json in sync with the credential it just installed,
// so there is no stale-identity case for this function to get wrong. An earlier
// version tried to paper over that case with a "last switch" marker; it made
// the wrong snapshot look current whenever ~/.claude.json happened to be
// rewritten by something else (which Claude Code does constantly, for reasons
// unrelated to identity) — refusing the switch upstream is what actually closes
// the gap, so the marker was removed rather than made more clever.
func currentAccountName() string {
	live := loadAccountEmail()
	if live == "" {
		return ""
	}
	names, err := snapshotAccountNames()
	if err != nil {
		return ""
	}
	for _, name := range names {
		if emailMatchesLive(snapshotAccountEmail(name), live) {
			return name
		}
	}
	return ""
}

// withAccountLock runs fn holding an exclusive advisory lock on
// ~/.claude/.account-switch.lock, so two concurrent switches (or a switch and a
// save) on this host serialize instead of interleaving their reads and writes.
//
// The lock file is opened per call rather than cached: flock locks are per open
// file description, so two goroutines sharing one *os.File would both "acquire"
// it and the lock would silently do nothing. Closing the file releases the lock,
// which is what the deferred Close guarantees even on a panic.
//
// What this cannot cover — a live Claude Code process rewriting the credential
// mid-switch — is documented in the design spec as an accepted residual window:
// this tool cannot lock a process it does not control, and every backup read
// still happens strictly before this tool's own overwrite.
func withAccountLock(fn func() error) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, accountLockFile), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", accountLockFile, err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

// switchAccount makes the named snapshot this host's active Claude Code account
// and returns the email that is live afterwards. The whole body runs under
// withAccountLock.
//
// The step order is the contract, not an implementation detail: nothing is read
// or written until the name is known; an already-active name returns without
// touching a single file (re-applying a snapshot would overwrite a live token
// that may have refreshed since the snapshot was captured with an older one);
// and the live credential is backed up unconditionally, then again under its own
// name when that name is known, both strictly before it is overwritten. A
// failure anywhere after that leaves at least one copy of the outgoing
// credential on disk, so no switch can ever strand an account with no way back.
func switchAccount(name string) (string, error) {
	var email string
	err := withAccountLock(func() error {
		var err error
		email, err = switchAccountLocked(name)
		return err
	})
	return email, err
}

// switchAccountLocked is switchAccount's body, already holding the lock. Split
// out so the lock scope is obvious and so tests can exercise the steps without
// re-entering the lock.
func switchAccountLocked(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// 1. Validate. Nothing below this point runs for an unknown name, and the
	// name is always one the directory listing produced — never a client string
	// interpolated into a path.
	names, err := snapshotAccountNames()
	if err != nil {
		return "", err
	}
	if !containsAccountName(names, name) {
		known := "none"
		if len(names) > 0 {
			known = strings.Join(names, ", ")
		}
		return "", fmt.Errorf("%w for %q (known: %s)", errUnknownAccount, name, known)
	}

	// 1.5. Require an identity snapshot before touching anything else. A
	// switch that installs a credential without also patching ~/.claude.json
	// leaves the identity cache pointing at the OUTGOING account while the
	// credential is the incoming one — and currentAccountName has no way to
	// tell, so a LATER switch reads that stale email, misidentifies which
	// snapshot is outgoing, and silently overwrites the wrong one. Refusing
	// here — before the rescue backup, before the credential write, before
	// anything — is what keeps currentAccountName's plain email matching
	// always accurate: every switch this tool performs now leaves identity in
	// sync with the credential it installs, with no exception to reason about.
	identity, err := os.ReadFile(snapshotAccountPath(home, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("snapshot %q has no identity snapshot — run 'claude-sessions account save %s' while logged into it first", name, name)
		}
		return "", fmt.Errorf("read identity snapshot %q: %w", name, err)
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(identity, &snapshot); err != nil {
		return "", fmt.Errorf("parse identity snapshot %q: %w", name, err)
	}
	// A file existing and parsing isn't enough on its own: identitySlice
	// writes an explicit JSON null for any key ~/.claude.json didn't have at
	// capture time (jq parity — see saveAccountSnapshot), so a snapshot saved
	// while not actually logged in can be a syntactically valid but
	// practically empty {"oauthAccount":null,"userID":null}. Applying that
	// would be a real patch that patches nothing (patchIdentityCache already
	// skips an explicit null so it never overwrites a good value with one),
	// recreating the same split state this precondition exists to prevent.
	//
	// The check is made against THIS SAME parsed map — not a second,
	// independent read/parse via snapshotAccountEmail — for two reasons:
	// snapshotAccountEmail unmarshals into a Go struct, which falls back to
	// case-insensitive key matching, while patchIdentityCache below looks
	// this map up by an exact "oauthAccount" key; a snapshot with any other
	// casing would satisfy a struct-based check while patchIdentityCache
	// silently found nothing to copy, reopening the exact split state this
	// precondition exists to close. Reading once and validating what will
	// actually be patched — not a byte-identical-assumed second read — also
	// closes a TOCTOU a concurrent (non-flock-holding, e.g. hand-editing the
	// file) writer could otherwise exploit between two separate reads.
	if strings.TrimSpace(identitySnapshotEmail(snapshot)) == "" {
		return "", fmt.Errorf("snapshot %q's identity snapshot has no email — run 'claude-sessions account save %s' while logged into it first", name, name)
	}

	// 1.6. Refuse if a previous switch was interrupted mid-flight (killed
	// between steps 5 and 6 below). See the pendingSwitchMarker doc for why
	// this exists: writing the credential and patching the identity are two
	// separate operations on two separate files/stores, so nothing can make
	// them atomic together — but a crash in that gap is exactly the split
	// state the identity precondition above exists to prevent, just reached a
	// different way. Without this check, the natural response to "the switch
	// didn't seem to take" — just running it again — would read the resulting
	// stale identity as "current" and back the wrong live credential up under
	// the wrong snapshot's name, corrupting it. Refusing until a human
	// confirms which account is actually live converts that silent corruption
	// into a loud, safe stop.
	if pending := readPendingSwitchMarker(home); pending != "" {
		return "", fmt.Errorf("a previous switch to %q did not finish (process interrupted?) — "+
			"run '/login' in Claude Code to confirm the live account and refresh its identity, then "+
			"'claude-sessions account save <that account's name>' to resync its snapshot and clear this warning "+
			"(save always clears it, since capturing what's live is exactly the confirmation this is waiting for); "+
			"or remove %s directly if you're certain nothing needs correcting", pending, pendingSwitchMarkerPath(home))
	}

	// 2. Already there: a true no-op. Not "harmless re-write" — re-applying the
	// snapshot would replace a possibly-refreshed live token with a stale one.
	current := currentAccountName()
	if current == name {
		return loadAccountEmail(), nil
	}

	// 3 + 4. Unconditional rescue copy, then the named sync-back when the
	// outgoing account is identifiable. Both complete before step 5.
	if err := backupOutgoing(home, current); err != nil {
		return "", err
	}

	// 4.5. Arm the pending-switch marker before the two operations that can't
	// be made atomic together. Best-effort: failing to write it is no worse
	// than not having this mitigation at all, so it doesn't block the switch
	// — but every effort is made to write it, since it's what turns a crash
	// in the next two steps into a safe refusal instead of a silent landmine
	// for whichever switch comes next.
	writePendingSwitchMarker(home, name)

	// 5. The switch itself.
	data, err := os.ReadFile(snapshotCredentialPath(home, name))
	if err != nil {
		return "", fmt.Errorf("read snapshot %q: %w", name, err)
	}
	if err := writeActiveCredential(data); err != nil {
		return "", err
	}

	// 6. Patch the identity cache so no /login is needed. Already read and
	// parsed at step 1.5 — re-reading here would just be a second trip to
	// disk for data validation already confirmed usable.
	if err := patchIdentityCache(snapshot); err != nil {
		return "", err
	}

	// 6.5. Disarm: both halves of the non-atomic pair completed, so there is
	// nothing left for the marker to guard against.
	clearPendingSwitchMarker(home)

	// 7. Report what is actually on disk now, not what was assumed.
	return loadAccountEmail(), nil
}

// pendingSwitchMarkerFile is armed for the duration of the one gap this file
// cannot close by construction — installing the new live credential and
// patching ~/.claude.json are separate writes to separate stores, so a crash
// between them can't be prevented, only detected. See switchAccountLocked
// steps 1.6/4.5/6.5.
const pendingSwitchMarkerFile = ".account-switch-pending"

func pendingSwitchMarkerPath(home string) string {
	return filepath.Join(home, ".claude", pendingSwitchMarkerFile)
}

// readPendingSwitchMarker returns the target name an interrupted switch was
// headed for, or "" if none is armed (the common case) or the marker is
// unreadable (treated as absent — a marker this tool itself can't read isn't
// one it can act on either, and failing open here would block every future
// switch over a problem this mechanism didn't cause).
func readPendingSwitchMarker(home string) string {
	data, err := os.ReadFile(pendingSwitchMarkerPath(home))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writePendingSwitchMarker arms the marker. Best-effort — see its call site.
func writePendingSwitchMarker(home, name string) {
	_ = writeFileAtomic(pendingSwitchMarkerPath(home), []byte(name), 0600)
}

// clearPendingSwitchMarker disarms the marker once both halves of the pair it
// guards have completed. Best-effort: a failed removal just means a future
// switch refuses and asks for manual confirmation it didn't strictly need to
// — safe, if annoying, and strictly better than the alternative of ever
// failing to arm it when it was needed.
func clearPendingSwitchMarker(home string) {
	_ = os.Remove(pendingSwitchMarkerPath(home))
}

// backupOutgoing performs switchAccount's steps 3 and 4: the unconditional
// rescue copy of whatever credential is live right now, then — only when the
// outgoing account resolved to a name — that account's own named credential and
// identity snapshots.
//
// A host with genuinely no live credential (os.ErrNotExist: never logged in on
// Linux, or a Keychain item that has never been created on macOS — see
// securityFindPassword's exit-code check) has nothing to lose, so the rescue
// copy is skipped rather than treated as a failure. Every OTHER read failure —
// permission denied, a locked keychain, a transient error — is refused instead,
// unconditionally, regardless of whether the outgoing account could be named:
// such an error means a live credential may well exist but simply couldn't be
// read right now, and proceeding would overwrite it with zero backup anywhere.
// This is the fix for a real gap an independent review found: the previous
// version only refused when `current != ""`, so an unreadable-but-present
// credential from an unidentified account was silently discarded.
func backupOutgoing(home, current string) error {
	live, readErr := readActiveCredential()
	if readErr == nil && len(live) == 0 {
		readErr = errors.New("live credential is empty")
	}
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return nil
		}
		if current != "" {
			return fmt.Errorf("refusing to switch: cannot back up the live credential for %q: %w", current, readErr)
		}
		return fmt.Errorf("refusing to switch: cannot back up the live credential: %w", readErr)
	}
	if err := writeSnapshotCredential(home, rescueSnapshotName, live); err != nil {
		return err
	}
	if current == "" {
		return nil
	}
	if err := writeSnapshotCredential(home, current, live); err != nil {
		return err
	}
	return writeIdentitySnapshot(home, current)
}

// containsAccountName reports whether names holds name.
func containsAccountName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// writeSnapshotCredential writes one credential snapshot, 0600, atomically. The
// bytes are stored exactly as read from the live credential — including the
// trailing newline `security -w` prints — so a snapshot this tool writes is
// byte-identical to one claude-switch's shell redirect writes.
func writeSnapshotCredential(home, name string, data []byte) error {
	return writeFileAtomic(snapshotCredentialPath(home, name), data, 0600)
}

// writeIdentitySnapshot captures ~/.claude.json's oauthAccount and userID into
// ~/.claude/.<name>.account.json, the same slice claude-switch's
// `jq '{oauthAccount, userID}'` produces — including an explicit null for a key
// the cache does not have, so the two tools' snapshots stay interchangeable.
func writeIdentitySnapshot(home, name string) error {
	data, err := os.ReadFile(identityCachePath(home))
	if err != nil {
		return fmt.Errorf("read identity cache: %w", err)
	}
	slice, err := identitySlice(data)
	if err != nil {
		return err
	}
	return writeFileAtomic(snapshotAccountPath(home, name), slice, 0600)
}

// identitySlice extracts exactly the identity keys from an identity cache,
// leaving every other key behind. Absent keys are written as null (jq parity);
// a map keeps the two keys in their alphabetical, and therefore stable, order.
func identitySlice(cache []byte) ([]byte, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(cache, &all); err != nil {
		return nil, fmt.Errorf("parse identity cache: %w", err)
	}
	out := make(map[string]json.RawMessage, len(identityCacheKeys))
	for _, key := range identityCacheKeys {
		v, ok := all[key]
		if !ok || len(v) == 0 {
			v = json.RawMessage("null")
		}
		out[key] = v
	}
	return json.MarshalIndent(out, "", "  ")
}

// identitySnapshotEmail extracts oauthAccount.emailAddress from an already-
// parsed identity snapshot map, by the exact "oauthAccount" key — the same
// lookup patchIdentityCache itself performs, so a caller validating "does
// this snapshot actually carry a usable email" is asking the identical
// question patchIdentityCache will act on, not a looser approximation of it.
// Returns "" for a missing key, an explicit null, or a value with no
// emailAddress — never an error; an unusable identity is this function's
// normal "no" answer, not a failure to determine one.
func identitySnapshotEmail(snapshot map[string]json.RawMessage) string {
	raw, ok := snapshot["oauthAccount"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var account struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := json.Unmarshal(raw, &account); err != nil {
		return ""
	}
	return account.EmailAddress
}

// patchIdentityCache merges a snapshot's identity keys into ~/.claude.json.
//
// The cache is decoded into a map[string]json.RawMessage so every key this
// codebase has no business touching — Claude Code's project history, settings,
// tips state — survives byte-for-byte. Only oauthAccount and userID are
// overwritten, and only when the snapshot actually carries them: a snapshot
// written before its account ever had a userID holds an explicit null there, and
// copying that null over a good value would strip the live cache of an id
// nothing else can restore.
//
// The write is atomic and preserves the file's existing mode, because
// ~/.claude.json belongs to Claude Code and a live process may be reading it.
func patchIdentityCache(snapshot map[string]json.RawMessage) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := identityCachePath(home)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read identity cache: %w", err)
	}
	var cache map[string]json.RawMessage
	if err := json.Unmarshal(data, &cache); err != nil {
		return fmt.Errorf("parse identity cache: %w", err)
	}
	if cache == nil {
		cache = map[string]json.RawMessage{}
	}
	for _, key := range identityCacheKeys {
		v, ok := snapshot[key]
		if !ok || len(v) == 0 || string(v) == "null" {
			continue
		}
		cache[key] = v
	}
	out, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	perm := os.FileMode(0600)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	return writeFileAtomic(path, out, perm)
}

// saveAccountSnapshot captures what is live right now — credential blob plus
// identity keys — into the named snapshot, overwriting any snapshot of that
// name. It is the explicit, user-invoked counterpart to switchAccount's
// automatic sync-back, and the automation of the manual two-command setup step
// claude-switch's header documents: re-run it after a relogin to refresh the
// snapshot.
//
// Local-only by design (there is no --server flag): it captures *this* machine's
// live credential, which is only meaningful on the machine holding it.
func saveAccountSnapshot(name string) error {
	if !accountNameOK(name) {
		return fmt.Errorf("invalid account name %q (allowed: letters, digits, '-', '_')", name)
	}
	return withAccountLock(func() error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		data, err := readActiveCredential()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return fmt.Errorf("no live credential to save")
		}
		if err := writeSnapshotCredential(home, name, data); err != nil {
			return err
		}
		if err := writeIdentitySnapshot(home, name); err != nil {
			return err
		}
		// Capturing what's live right now, under a name, is exactly the
		// ground-truth-reestablishing action the pending-switch marker is
		// waiting for — clearing it here (rather than only ever from inside
		// switchAccountLocked) is what makes the marker's own refusal message
		// (below) a complete, self-contained recovery step instead of a
		// two-part instruction the user could stop halfway through.
		clearPendingSwitchMarker(home)
		return nil
	})
}
