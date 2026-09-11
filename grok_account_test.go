package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type grokAccountFixture struct {
	t    *testing.T
	home string
}

func grokAuthJSON(email, key, refresh, clientID, userID string) []byte {
	return []byte(`{
  "https://auth.x.ai::default": {
    "key": "` + key + `",
    "email": "` + email + `",
    "refresh_token": "` + refresh + `",
    "oidc_client_id": "` + clientID + `",
    "user_id": "` + userID + `"
  }
}`)
}

func newGrokAccountFixture(t *testing.T) *grokAccountFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GROK_HOME", home)
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	prevAlive := grokPIDAlive
	grokPIDAlive = func(int) bool { return false }
	t.Cleanup(func() { grokPIDAlive = prevAlive })
	return &grokAccountFixture{t: t, home: home}
}

func (f *grokAccountFixture) writeLive(email, key, refresh, clientID, userID string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.home, "auth.json"), grokAuthJSON(email, key, refresh, clientID, userID), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *grokAccountFixture) live() string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.home, "auth.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func stubGrokRefresh(t *testing.T, fn func(refreshToken, clientID string) (*grokTokenResponse, error)) {
	t.Helper()
	prev := grokTokenRefresh
	grokTokenRefresh = fn
	t.Cleanup(func() { grokTokenRefresh = prev })
}

func TestCmdAccountRequiresTool(t *testing.T) {
	for _, args := range [][]string{
		{"save", "x"},
		{"list"},
		{"switch", "x"},
	} {
		stderr := captureStderr(t, func() {
			if got := cmdAccount(args); got != 2 {
				t.Errorf("cmdAccount(%v) exit = %d, want 2", args, got)
			}
		})
		if !strings.Contains(stderr, "tool required") {
			t.Errorf("cmdAccount(%v) stderr = %q, want tool required", args, stderr)
		}
	}
}

func TestCmdAccountGrokNoVerb(t *testing.T) {
	stderr := captureStderr(t, func() {
		if got := cmdAccount([]string{"grok"}); got != 2 {
			t.Errorf("exit = %d, want 2", got)
		}
	})
	if !strings.Contains(stderr, accountUsageMsg) {
		t.Errorf("stderr = %q, want usage", stderr)
	}
}

func TestCmdAccountUnknownTool(t *testing.T) {
	stderr := captureStderr(t, func() {
		if got := cmdAccount([]string{"nope", "save", "x"}); got != 2 {
			t.Errorf("exit = %d, want 2", got)
		}
	})
	if !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("stderr = %q, want unknown subcommand", stderr)
	}
}

func TestCmdAccountClaudeSaveStillWorks(t *testing.T) {
	f := newAccountFixture(t)
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	if got := cmdAccount([]string{"claude", "save", "avisoma"}); got != 0 {
		t.Fatalf("exit = %d, want 0", got)
	}
	if got := snapshotAccountEmail("avisoma"); got != "andy@avisoma.com" {
		t.Fatalf("email = %q", got)
	}
}

func TestGrokAccountSaveRoundTrip(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("me@x.ai", "tok-live", "rt-live", "client-1", "uid-1")
	if got := cmdAccount([]string{"grok", "save", "work"}); got != 0 {
		t.Fatalf("exit = %d, want 0", got)
	}
	auth := filepath.Join(f.home, "claude-sessions", "accounts", "work.auth.json")
	ident := filepath.Join(f.home, "claude-sessions", "accounts", "work.account.json")
	if _, err := os.Stat(auth); err != nil {
		t.Fatalf("auth snapshot missing: %v", err)
	}
	if _, err := os.Stat(ident); err != nil {
		t.Fatalf("identity snapshot missing: %v", err)
	}
	if got := grokSnapshotEmail("work"); got != "me@x.ai" {
		t.Fatalf("email = %q", got)
	}
}

func TestGrokAccountSaveRefusesEmailMismatch(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("old@x.ai", "tok-old", "rt-old", "client-1", "uid-old")
	if err := saveGrokAccountSnapshot("work", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("new@x.ai", "tok-new", "rt-new", "client-1", "uid-new")
	err := saveGrokAccountSnapshot("work", false)
	if err == nil {
		t.Fatal("want a refusal without --force")
	}
	if !strings.Contains(err.Error(), "old@x.ai") || !strings.Contains(err.Error(), "new@x.ai") {
		t.Fatalf("err = %v, want both emails", err)
	}
	if err := saveGrokAccountSnapshot("work", true); err != nil {
		t.Fatal(err)
	}
	if got := grokSnapshotEmail("work"); got != "new@x.ai" {
		t.Fatalf("email = %q after --force", got)
	}
}

func TestGrokAccountSwitchInstallsAndNoOps(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("b@x.ai", "tok-b", "rt-b", "client-1", "uid-b")
	if err := saveGrokAccountSnapshot("beta", false); err != nil {
		t.Fatal(err)
	}
	stubGrokRefresh(t, func(string, string) (*grokTokenResponse, error) {
		return &grokTokenResponse{AccessToken: "tok-a-rot", RefreshToken: "rt-a-rot", ExpiresIn: 3600}, nil
	})
	email, _, err := switchGrokAccount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if email != "a@x.ai" {
		t.Fatalf("email = %q", email)
	}
	doc, err := parseGrokAuth([]byte(f.live()))
	if err != nil {
		t.Fatal(err)
	}
	if doc.key() != "tok-a-rot" {
		t.Fatalf("live key = %q, want rotated token", doc.key())
	}
	calls := 0
	stubGrokRefresh(t, func(string, string) (*grokTokenResponse, error) {
		calls++
		return nil, nil
	})
	email, warnings, err := switchGrokAccount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if email != "a@x.ai" {
		t.Fatalf("noop email = %q", email)
	}
	if calls != 0 {
		t.Fatalf("refresh calls = %d, want 0 on no-op", calls)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none on no-op", warnings)
	}
}

func TestGrokAccountSwitchInvalidGrantRefuses(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("b@x.ai", "tok-b", "rt-b", "client-1", "uid-b")
	if err := saveGrokAccountSnapshot("beta", false); err != nil {
		t.Fatal(err)
	}
	stubGrokRefresh(t, func(string, string) (*grokTokenResponse, error) {
		return nil, &grokOAuthError{Status: 400, Code: "invalid_grant"}
	})
	before := f.live()
	_, _, err := switchGrokAccount("alpha")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "account grok save") {
		t.Fatalf("err = %v, want grok save", err)
	}
	if f.live() != before {
		t.Fatal("live auth.json moved on invalid_grant")
	}
}

func TestGrokOAuthRefreshUpdatesKey(t *testing.T) {
	data := grokAuthJSON("me@x.ai", "old-key", "rt", "client-1", "uid")
	stubGrokRefresh(t, func(refreshToken, clientID string) (*grokTokenResponse, error) {
		if refreshToken != "rt" || clientID != "client-1" {
			t.Fatalf("refresh_token=%q client_id=%q", refreshToken, clientID)
		}
		return &grokTokenResponse{AccessToken: "new-key", RefreshToken: "new-rt", ExpiresIn: 60}, nil
	})
	got, err := rotateGrokAuthBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseGrokAuth(got)
	if err != nil {
		t.Fatal(err)
	}
	if doc.key() != "new-key" {
		t.Fatalf("key = %q, want new-key", doc.key())
	}
	if doc.refreshToken() != "new-rt" {
		t.Fatalf("refresh = %q, want new-rt", doc.refreshToken())
	}
}

func TestGrokAccountRemoveLiveNeedsYes(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("me@x.ai", "tok", "rt", "client-1", "uid")
	if err := saveGrokAccountSnapshot("work", false); err != nil {
		t.Fatal(err)
	}
	if got := cmdGrokAccountRemove([]string{"work"}); got != 1 {
		t.Fatalf("exit = %d, want a refusal", got)
	}
	if names, _ := grokSnapshotNames(); len(names) != 1 {
		t.Fatalf("names = %v, want kept", names)
	}
	if got := cmdGrokAccountRemove([]string{"work", "-y"}); got != 0 {
		t.Fatalf("exit with -y = %d, want 0", got)
	}
	if names, _ := grokSnapshotNames(); len(names) != 0 {
		t.Fatalf("names = %v, want none", names)
	}
	if !strings.Contains(f.live(), "me@x.ai") {
		t.Fatal("remove must not touch live auth.json")
	}
}

func TestGrokAccountListMarksActive(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("b@x.ai", "tok-b", "rt-b", "client-1", "uid-b")
	if err := saveGrokAccountSnapshot("beta", false); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if got := cmdAccount([]string{"grok", "list"}); got != 0 {
			t.Fatalf("exit = %d", got)
		}
	})
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("list missing names:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "beta") && !strings.Contains(line, "yes") {
			t.Fatalf("beta should be active: %q", line)
		}
		if strings.Contains(line, "alpha") && strings.Contains(line, "yes") {
			t.Fatalf("alpha should not be active: %q", line)
		}
	}
}

func TestGrokAccountSwitchWarnsAboutLiveSessions(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("b@x.ai", "tok-b", "rt-b", "client-1", "uid-b")
	if err := saveGrokAccountSnapshot("beta", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, grokActiveSessionsFile), []byte(`[{"session_id":"s1","pid":4242}]`), 0600); err != nil {
		t.Fatal(err)
	}
	prev := grokPIDAlive
	grokPIDAlive = func(pid int) bool { return pid == 4242 }
	t.Cleanup(func() { grokPIDAlive = prev })
	stubGrokRefresh(t, func(string, string) (*grokTokenResponse, error) {
		return &grokTokenResponse{AccessToken: "tok-a-rot", RefreshToken: "rt-a", ExpiresIn: 60}, nil
	})
	_, warnings, err := switchGrokAccount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "follow the new login") || !strings.Contains(warnings[0], "4242") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestUsageHandlerIncludesGrokKnownAccounts(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("b@x.ai", "tok-b", "rt-b", "client-1", "uid-b")
	if err := saveGrokAccountSnapshot("beta", false); err != nil {
		t.Fatal(err)
	}
	s := &server{token: "secret"}
	req := httptest.NewRequest("GET", "/usage", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.usage(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var resp usageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.GrokActiveSnapshotName != "beta" {
		t.Fatalf("active = %q, want beta", resp.GrokActiveSnapshotName)
	}
	if len(resp.GrokKnownAccounts) != 1 || resp.GrokKnownAccounts[0].Name != "alpha" {
		t.Fatalf("known = %+v, want alpha only", resp.GrokKnownAccounts)
	}
	if resp.GrokAccount != "b@x.ai" {
		t.Fatalf("grokAccount = %q, want live email b@x.ai", resp.GrokAccount)
	}
}

func TestGrokAccountRowsFromUsesUsageEmailWhenGrokUsageNil(t *testing.T) {
	got := grokAccountRowsFrom("box", accountSnapshot{
		GrokAccount:    "live@x.ai",
		GrokActiveName: "beta",
		GrokKnown:      []KnownAccountUsage{{Name: "alpha", Account: "parked@x.ai"}},
	})
	want := []accountRow{
		{Host: "box", Name: "alpha", Email: "parked@x.ai", Tool: accountToolGrok},
		{Host: "box", Name: "beta", Email: "live@x.ai", Active: true, Tool: accountToolGrok},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTryRotateGrokSnapshotSkipsLiveGrant(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("b@x.ai", "tok-b", "rt-b", "client-1", "uid-b")
	if err := saveGrokAccountSnapshot("beta", false); err != nil {
		t.Fatal(err)
	}
	stubGrokRefresh(t, func(string, string) (*grokTokenResponse, error) {
		return &grokTokenResponse{AccessToken: "tok-a-rot", RefreshToken: "rt-a-rot", ExpiresIn: 60}, nil
	})
	if _, _, err := switchGrokAccount("alpha"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	stubGrokRefresh(t, func(string, string) (*grokTokenResponse, error) {
		calls++
		t.Fatal("must not rotate a parked copy of the live grant")
		return nil, nil
	})
	tok := tryRotateGrokSnapshot("alpha")
	if tok != "tok-a-rot" {
		t.Fatalf("tok = %q, want the live access token, not a new rotate", tok)
	}
	if calls != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls)
	}
}

func TestTryRotateGrokSnapshotSkipsWhenLiveEmailUnknown(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	stubGrokRefresh(t, func(string, string) (*grokTokenResponse, error) {
		calls++
		return &grokTokenResponse{AccessToken: "burned", RefreshToken: "burned-rt", ExpiresIn: 60}, nil
	})
	tok := tryRotateGrokSnapshot("alpha")
	if tok != "tok-a" {
		t.Fatalf("tok = %q, want the parked access token without a rotate", tok)
	}
	if calls != 0 {
		t.Fatalf("refresh calls = %d, want 0 when live email is unknown", calls)
	}
}

func TestSwitchGrokAccountRefusesIdentityAuthMismatch(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	f.writeLive("b@x.ai", "tok-b", "rt-b", "client-1", "uid-b")
	ident, err := grokSnapshotIdentityPath("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ident, []byte(`{"email":"other@x.ai","user_id":"uid-a"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := f.live()
	_, _, err = switchGrokAccount("alpha")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want identity/auth mismatch", err)
	}
	if f.live() != before {
		t.Fatal("live auth.json moved on a mismatch")
	}
}

func TestGrokSnapshotGrantEmailFallsBackPastMissingIdentity(t *testing.T) {
	f := newGrokAccountFixture(t)
	f.writeLive("a@x.ai", "tok-a", "rt-a", "client-1", "uid-a")
	if err := saveGrokAccountSnapshot("alpha", false); err != nil {
		t.Fatal(err)
	}
	ident, err := grokSnapshotIdentityPath("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ident); err != nil {
		t.Fatal(err)
	}
	if got := grokSnapshotEmail("alpha"); got != "" {
		t.Fatalf("identity email = %q, want empty", got)
	}
	if got := grokSnapshotGrantEmail("alpha"); got != "a@x.ai" {
		t.Fatalf("grant email = %q, want a@x.ai from auth.json", got)
	}
}
