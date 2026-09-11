package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Grok account snapshots live under $GROK_HOME/claude-sessions/ so the Grok
// CLI never lists them. Identity is inside auth.json itself (unlike Claude,
// which splits credential + ~/.claude.json), so a switch is one atomic write
// and there is no pending-switch marker.

const grokAccountLockFile = "account-switch.lock"

func grokHome() (string, error) {
	if v := os.Getenv("GROK_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, grokDir), nil
}

func grokAuthPath() (string, error) {
	home, err := grokHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "auth.json"), nil
}

func grokSessionsRoot() (string, error) {
	home, err := grokHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "claude-sessions"), nil
}

func grokAccountsDir() (string, error) {
	root, err := grokSessionsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "accounts"), nil
}

func grokSnapshotAuthPath(name string) (string, error) {
	dir, err := grokAccountsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".auth.json"), nil
}

func grokSnapshotIdentityPath(name string) (string, error) {
	dir, err := grokAccountsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".account.json"), nil
}

// withGrokAccountLock runs fn holding an exclusive advisory lock on
// $GROK_HOME/claude-sessions/account-switch.lock. The lock file is opened per
// call: flock is per open file description, so a cached handle would let two
// goroutines in the same process both "acquire" it.
func withGrokAccountLock(fn func() error) error {
	root, err := grokSessionsRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, grokAccountLockFile), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", grokAccountLockFile, err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

// grokAuthDoc is one auth.json blob with the preferred issuer entry selected
// the same way loadGrokAuth prefers https://auth.x.ai:: then the first
// non-empty key.
type grokAuthDoc struct {
	issuers map[string]json.RawMessage
	issuer  string
	entry   map[string]json.RawMessage
}

func parseGrokAuth(data []byte) (*grokAuthDoc, error) {
	var issuers map[string]json.RawMessage
	if err := json.Unmarshal(data, &issuers); err != nil {
		return nil, fmt.Errorf("parse grok auth: %w", err)
	}
	var preferred, fallback string
	for issuer, raw := range issuers {
		entry, err := unmarshalGrokEntry(raw)
		if err != nil || grokEntryString(entry, "key") == "" {
			continue
		}
		if strings.HasPrefix(issuer, grokPreferredIssuerPrefix) {
			preferred = issuer
			continue
		}
		if fallback == "" {
			fallback = issuer
		}
	}
	issuer := preferred
	if issuer == "" {
		issuer = fallback
	}
	if issuer == "" {
		return nil, fmt.Errorf("no grok access token")
	}
	entry, err := unmarshalGrokEntry(issuers[issuer])
	if err != nil {
		return nil, err
	}
	return &grokAuthDoc{issuers: issuers, issuer: issuer, entry: entry}, nil
}

func unmarshalGrokEntry(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("parse grok auth entry: %w", err)
	}
	if entry == nil {
		entry = map[string]json.RawMessage{}
	}
	return entry, nil
}

func grokEntryString(entry map[string]json.RawMessage, key string) string {
	raw, ok := entry[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

func (d *grokAuthDoc) key() string          { return grokEntryString(d.entry, "key") }
func (d *grokAuthDoc) email() string        { return grokEntryString(d.entry, "email") }
func (d *grokAuthDoc) userID() string       { return grokEntryString(d.entry, "user_id") }
func (d *grokAuthDoc) refreshToken() string { return grokEntryString(d.entry, "refresh_token") }
func (d *grokAuthDoc) oidcClientID() string { return grokEntryString(d.entry, "oidc_client_id") }

func (d *grokAuthDoc) setString(key, val string) {
	b, err := json.Marshal(val)
	if err != nil {
		return
	}
	d.entry[key] = b
}

func (d *grokAuthDoc) patchTokens(tok *grokTokenResponse) ([]byte, error) {
	d.setString("key", tok.AccessToken)
	if tok.RefreshToken != "" {
		d.setString("refresh_token", tok.RefreshToken)
	}
	if tok.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
		d.setString("expires_at", exp)
	}
	entryRaw, err := json.Marshal(d.entry)
	if err != nil {
		return nil, err
	}
	d.issuers[d.issuer] = entryRaw
	return json.Marshal(d.issuers)
}

func liveGrokEmail() string {
	_, email, err := loadGrokAuth()
	if err != nil {
		return ""
	}
	return email
}

func grokSnapshotNames() ([]string, error) {
	dir, err := grokAccountsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	const suffix = ".auth.json"
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		base := e.Name()
		if !strings.HasSuffix(base, suffix) || len(base) <= len(suffix) {
			continue
		}
		name := base[:len(base)-len(suffix)]
		if name == rescueSnapshotName {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func grokSnapshotEmail(name string) string {
	path, err := grokSnapshotIdentityPath(name)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var ident struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(data, &ident); err != nil {
		return ""
	}
	return ident.Email
}

// grokSnapshotGrantEmail is the email on the snapshot's auth.json preferred
// entry, falling back to the identity file. The known-accounts skip and
// parked rotate must use this, not the identity file alone: a save that
// wrote auth.json and then failed the identity write would otherwise look
// unowned and get rotated while still being the live grant.
func grokSnapshotGrantEmail(name string) string {
	path, err := grokSnapshotAuthPath(name)
	if err != nil {
		return grokSnapshotEmail(name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return grokSnapshotEmail(name)
	}
	doc, err := parseGrokAuth(data)
	if err != nil {
		return grokSnapshotEmail(name)
	}
	if email := strings.TrimSpace(doc.email()); email != "" {
		return email
	}
	return grokSnapshotEmail(name)
}

func grokCurrentAccountName() string {
	live := liveGrokEmail()
	if live == "" {
		return ""
	}
	names, err := grokSnapshotNames()
	if err != nil {
		return ""
	}
	for _, name := range names {
		if emailMatchesLive(grokSnapshotEmail(name), live) {
			return name
		}
	}
	return ""
}

func writeGrokSnapshotAuth(name string, data []byte) error {
	path, err := grokSnapshotAuthPath(name)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0600)
}

func writeGrokSnapshotIdentity(name, email, userID string) error {
	path, err := grokSnapshotIdentityPath(name)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(struct {
		Email  string `json:"email"`
		UserID string `json:"user_id"`
	}{Email: email, UserID: userID}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, raw, 0600)
}

func grokSnapshotAccessToken(name string) (string, error) {
	path, err := grokSnapshotAuthPath(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	doc, err := parseGrokAuth(data)
	if err != nil {
		return "", err
	}
	return doc.key(), nil
}

func saveGrokAccountSnapshot(name string, force bool) error {
	if !accountNameOK(name) {
		return fmt.Errorf("invalid account name %q (allowed: letters, digits, '-', '_')", name)
	}
	return withGrokAccountLock(func() error {
		path, err := grokAuthPath()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("no live grok auth.json to save: %w", err)
		}
		doc, err := parseGrokAuth(data)
		if err != nil {
			return err
		}
		if doc.key() == "" {
			return fmt.Errorf("no grok access token")
		}
		if !force {
			if existing := grokSnapshotEmail(name); existing != "" {
				if live := liveGrokEmail(); live != "" && !strings.EqualFold(existing, live) {
					return fmt.Errorf("snapshot %q stands for %s but %s is logged in — saving would file this "+
						"account's credential under the other one's name; switch to %s first, or pass --force if "+
						"the snapshot really should be reassigned", name, existing, live, existing)
				}
			}
		}
		if err := writeGrokSnapshotAuth(name, data); err != nil {
			return err
		}
		return writeGrokSnapshotIdentity(name, doc.email(), doc.userID())
	})
}

func planGrokAccountRemoval(name string) (accountRemovalPlan, error) {
	names, err := grokSnapshotNames()
	if err != nil {
		return accountRemovalPlan{}, err
	}
	if !containsAccountName(names, name) {
		known := "none"
		if len(names) > 0 {
			known = strings.Join(names, ", ")
		}
		return accountRemovalPlan{}, fmt.Errorf("%w for %q (known: %s)", errUnknownAccount, name, known)
	}
	return accountRemovalPlan{
		Name: name,
		Live: emailMatchesLive(grokSnapshotEmail(name), liveGrokEmail()),
	}, nil
}

func removeGrokAccountSnapshot(name string) ([]string, bool, error) {
	var removed []string
	var wasLive bool
	err := withGrokAccountLock(func() error {
		names, err := grokSnapshotNames()
		if err != nil {
			return err
		}
		if !containsAccountName(names, name) {
			known := "none"
			if len(names) > 0 {
				known = strings.Join(names, ", ")
			}
			return fmt.Errorf("%w for %q (known: %s)", errUnknownAccount, name, known)
		}
		wasLive = emailMatchesLive(grokSnapshotEmail(name), liveGrokEmail())
		for _, pathFn := range []func(string) (string, error){grokSnapshotAuthPath, grokSnapshotIdentityPath} {
			path, err := pathFn(name)
			if err != nil {
				return err
			}
			base := filepath.Base(path)
			err = os.Remove(path)
			if err == nil {
				removed = append(removed, base)
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", base, err)
			}
		}
		return nil
	})
	return removed, wasLive, err
}

func switchGrokAccount(name string) (string, []string, error) {
	var email string
	var warnings []string
	err := withGrokAccountLock(func() error {
		var err error
		email, warnings, err = switchGrokAccountLocked(name)
		return err
	})
	return email, warnings, err
}

func switchGrokAccountLocked(name string) (string, []string, error) {
	names, err := grokSnapshotNames()
	if err != nil {
		return "", nil, err
	}
	if !containsAccountName(names, name) {
		known := "none"
		if len(names) > 0 {
			known = strings.Join(names, ", ")
		}
		return "", nil, fmt.Errorf("%w for %q (known: %s)", errUnknownAccount, name, known)
	}

	identPath, err := grokSnapshotIdentityPath(name)
	if err != nil {
		return "", nil, err
	}
	ident, err := os.ReadFile(identPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("snapshot %q has no identity snapshot — run 'claude-sessions account grok save %s' while logged into it first", name, name)
		}
		return "", nil, fmt.Errorf("read identity snapshot %q: %w", name, err)
	}
	var identObj struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(ident, &identObj); err != nil {
		return "", nil, fmt.Errorf("parse identity snapshot %q: %w", name, err)
	}
	snapEmail := strings.TrimSpace(identObj.Email)
	if snapEmail == "" {
		return "", nil, fmt.Errorf("snapshot %q's identity snapshot has no email — run 'claude-sessions account grok save %s' while logged into it first", name, name)
	}

	if emailMatchesLive(snapEmail, liveGrokEmail()) {
		return snapEmail, nil, nil
	}

	authPath, err := grokSnapshotAuthPath(name)
	if err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(authPath)
	if err != nil {
		return "", nil, fmt.Errorf("read snapshot %q: %w", name, err)
	}
	doc, err := parseGrokAuth(data)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	if doc.key() == "" {
		return "", nil, fmt.Errorf("snapshot %q has no access token", name)
	}
	if strings.TrimSpace(doc.refreshToken()) == "" {
		return "", nil, fmt.Errorf("snapshot %q has no refresh token, so installing it would log this host out — run 'claude-sessions account grok save %s' while logged into that account", name, name)
	}
	authEmail := strings.TrimSpace(doc.email())
	if authEmail == "" || !strings.EqualFold(authEmail, snapEmail) {
		return "", nil, fmt.Errorf("snapshot %q identity email %s does not match auth.json email %s — run 'claude-sessions account grok save %s' while logged into that account", name, snapEmail, authEmail, name)
	}

	warnings := grokSwitchSessionWarnings(name)

	data, err = rotateGrokSnapshotForSwitch(name, data)
	if err != nil {
		return "", nil, err
	}

	if err := backupOutgoingGrok(); err != nil {
		return "", warnings, err
	}

	livePath, err := grokAuthPath()
	if err != nil {
		return "", warnings, err
	}
	if err := writeFileAtomic(livePath, data, 0600); err != nil {
		return "", warnings, err
	}
	return snapEmail, warnings, nil
}

func backupOutgoingGrok() error {
	livePath, err := grokAuthPath()
	if err != nil {
		return err
	}
	live, err := os.ReadFile(livePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := writeGrokSnapshotAuth(rescueSnapshotName, live); err != nil {
		return err
	}
	if current := grokCurrentAccountName(); current != "" {
		if err := writeGrokSnapshotAuth(current, live); err != nil {
			return err
		}
	}
	return nil
}

func grokSwitchSessionWarnings(name string) []string {
	pids := grokLivePIDs()
	if len(pids) == 0 {
		return nil
	}
	return []string{grokRunningSessionsWarning(name, pids)}
}

func grokLivePIDs() []int {
	home, err := grokHome()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, grokActiveSessionsFile))
	if err != nil {
		return nil
	}
	var entries []grokActiveSession
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		if e.PID == 0 || !grokPIDAlive(e.PID) {
			continue
		}
		pids = append(pids, e.PID)
	}
	sort.Ints(pids)
	return pids
}

func grokRunningSessionsWarning(name string, pids []int) string {
	shown := pids
	extra := 0
	if len(shown) > switchWarningPIDLimit {
		extra = len(shown) - switchWarningPIDLimit
		shown = shown[:switchWarningPIDLimit]
	}
	parts := make([]string, len(shown))
	for i, p := range shown {
		parts[i] = strconv.Itoa(p)
	}
	list := strings.Join(parts, ", ")
	if extra > 0 {
		list = fmt.Sprintf("%s and %d more", list, extra)
	}
	noun := "session is"
	if len(pids) != 1 {
		noun = "sessions are"
	}
	return fmt.Sprintf("%d Grok %s still running (pid %s). Running Grok sessions will follow the new login on the next API call. Close them and re-run 'claude-sessions account grok switch %s' if you did not mean to switch them.",
		len(pids), noun, list, name)
}

func tryRotateGrokSnapshot(name string) string {
	var tok string
	_ = withGrokAccountLock(func() error {
		path, err := grokSnapshotAuthPath(name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		doc, err := parseGrokAuth(data)
		if err != nil {
			return err
		}
		if doc.key() == "" {
			return fmt.Errorf("no access token")
		}
		// Re-check under the lock. fetchKnownGrokAccounts skipped this name
		// against a live email captured at the start of the pass; a switch
		// to this account can finish in between and leave this snapshot's
		// refresh token as the live grant. Rotating it here would
		// invalidate the auth.json copy Grok is using.
		//
		// An unreadable live email is not evidence this is a different
		// account — refuse the rotate rather than guess.
		live := liveGrokEmail()
		if live == "" || emailMatchesLive(grokSnapshotGrantEmail(name), live) {
			tok = doc.key()
			return nil
		}
		refreshed, err := rotateGrokAuthBytes(data)
		if err != nil {
			return err
		}
		if err := writeGrokSnapshotAuth(name, refreshed); err != nil {
			newDoc, perr := parseGrokAuth(refreshed)
			if perr == nil {
				tok = newDoc.key()
			}
			return err
		}
		newDoc, err := parseGrokAuth(refreshed)
		if err != nil {
			return err
		}
		tok = newDoc.key()
		return nil
	})
	return tok
}
