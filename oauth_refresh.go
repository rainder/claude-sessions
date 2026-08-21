package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Claude Code's OAuth token endpoint (verified against 2.1.233). Parked
// snapshot rotation talks to this directly — there is no `claude` binary on
// the path we can shell out to, and the live Keychain item is still only
// written by writeActiveCredential on switch.
const oauthTokenURL = "https://platform.claude.com/v1/oauth/token"

// oauthClientID is Claude Code's public OAuth client. Sent in the refresh
// body; never written into a snapshot that did not already have a clientId.
const oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// oauthDefaultScopes is Claude Code's z9e default, used when the blob has no
// scopes array (or an empty one). If the blob has scopes, those are sent
// instead — joining a different set would shrink or widen the grant.
var oauthDefaultScopes = []string{
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

const oauthRefreshTimeout = 30 * time.Second

// oauthTokenResponse is the JSON the token endpoint returns (snake_case).
type oauthTokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
}

// oauthRefreshError is a non-200 answer from the token endpoint. Status is
// the HTTP code; Code is the OAuth `error` field when the body had one.
type oauthRefreshError struct {
	Status int
	Code   string
}

func (e *oauthRefreshError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("oauth token: HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("oauth token: HTTP %d", e.Status)
}

// isInvalidGrant reports a dead refresh token. Only the OAuth error code
// decides this: a bare HTTP 400 can be invalid_scope or invalid_request
// (a bug in our body, or scopes the grant never had), and treating those
// as a dead grant would refuse every switch until a human re-saves. Other
// failures (network, 5xx, 400-without-this-code) are transient and must
// not be treated as "this snapshot can never log in again".
func isInvalidGrant(err error) bool {
	var e *oauthRefreshError
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == "invalid_grant"
}

// oauthTokenRefresh is the token-endpoint seam. Production points at
// refreshOAuthToken; tests swap it. TestMain defaults it to a panic so a
// forgotten override cannot spend a real refresh token.
var oauthTokenRefresh = refreshOAuthToken

// refreshOAuthToken POSTs a refresh_token grant to Claude Code's token
// endpoint. No Authorization header, no anthropic-beta — this endpoint is
// the one that *issues* bearer tokens, not the one that spends them.
func refreshOAuthToken(refreshToken string, scopes []string) (*oauthTokenResponse, error) {
	reqBody, err := json.Marshal(struct {
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
		Scope        string `json:"scope"`
	}{
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
		ClientID:     oauthClientID,
		Scope:        strings.Join(scopes, " "),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: oauthRefreshTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var raw struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &raw)
		return nil, &oauthRefreshError{Status: resp.StatusCode, Code: raw.Error}
	}
	var tok oauthTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("parse oauth token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("oauth token response missing access_token")
	}
	if tok.ExpiresIn <= 0 {
		return nil, fmt.Errorf("oauth token response missing expires_in")
	}
	return &tok, nil
}

// refreshOAuthCredential rotates one credential blob in memory: parse, call
// the token seam, patch, return new bytes. No file I/O and no lock — callers
// that own a snapshot file persist themselves (and the lock is the caller's
// too: switch already holds it, the poller takes it in rotateParkedSnapshotAccess).
//
// Patching goes through map[string]json.RawMessage at both the outer blob
// and the claudeAiOauth object so unknown keys (mcpOAuth, subscriptionType,
// rateLimitTier, anything Claude Code adds later) survive. Re-marshalling
// through oauthCredentials would drop them.
func refreshOAuthCredential(data []byte) ([]byte, error) {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	innerRaw, ok := outer["claudeAiOauth"]
	if !ok || len(innerRaw) == 0 || string(innerRaw) == "null" {
		return nil, fmt.Errorf("parse credentials: no claudeAiOauth")
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(innerRaw, &inner); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	refreshToken, err := jsonRawString(inner["refreshToken"])
	if err != nil {
		return nil, fmt.Errorf("parse credentials: refreshToken: %w", err)
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("parse credentials: no refresh token")
	}
	resp, err := oauthTokenRefresh(refreshToken, oauthScopesFromBlob(inner["scopes"]))
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.AccessToken == "" {
		return nil, fmt.Errorf("oauth token response missing access_token")
	}
	if resp.ExpiresIn <= 0 {
		return nil, fmt.Errorf("oauth token response missing expires_in")
	}
	now := time.Now()
	if inner["accessToken"], err = marshalJSON(resp.AccessToken); err != nil {
		return nil, err
	}
	if resp.RefreshToken != "" {
		if inner["refreshToken"], err = marshalJSON(resp.RefreshToken); err != nil {
			return nil, err
		}
	}
	if inner["expiresAt"], err = marshalJSON(now.Add(time.Duration(resp.ExpiresIn) * time.Second).UnixMilli()); err != nil {
		return nil, err
	}
	if resp.RefreshTokenExpiresIn > 0 {
		if inner["refreshTokenExpiresAt"], err = marshalJSON(now.Add(time.Duration(resp.RefreshTokenExpiresIn) * time.Second).UnixMilli()); err != nil {
			return nil, err
		}
	}
	if resp.Scope != "" {
		if inner["scopes"], err = marshalJSON(strings.Fields(resp.Scope)); err != nil {
			return nil, err
		}
	}
	patched, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	outer["claudeAiOauth"] = patched
	return json.Marshal(outer)
}

// refreshSnapshotCredential reads a parked snapshot, rotates it, and writes
// the new blob back. No lock — the caller holds it (switchAccountLocked, or
// rotateParkedSnapshotAccess). On a successful rotate the write always runs
// before this returns: a crash between POST and write can burn a rotated
// refresh token, the same residual Claude Code itself accepts. A persist
// failure after a successful rotate returns that error so the poller does
// not treat the in-memory grant as saved.
func refreshSnapshotCredential(home, name string) ([]byte, error) {
	data, err := os.ReadFile(snapshotCredentialPath(home, name))
	if err != nil {
		return nil, fmt.Errorf("read snapshot %q: %w", name, err)
	}
	refreshed, err := refreshOAuthCredential(data)
	if err != nil {
		return nil, err
	}
	if err := writeSnapshotCredential(home, name, refreshed); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// rotateSnapshotForSwitch is switchAccountLocked's rotate step: always
// attempted (even when access is still valid — the point is that Claude
// Code starts with a fresh token), still inside the caller's lock, and
// strictly before backupOutgoing / the pending-switch marker so a failed
// rotate cannot leave the host mid-switch.
//
// invalid_grant refuses the switch. Any other error keeps the original
// bytes, matching today's "expired access + good refresh still installs".
// A persist failure after a successful rotate still returns the new bytes
// so writeActiveCredential can install the grant rather than throw it away.
func rotateSnapshotForSwitch(home, name string, data []byte) ([]byte, error) {
	refreshed, rerr := refreshOAuthCredential(data)
	if rerr == nil {
		if werr := writeSnapshotCredential(home, name, refreshed); werr != nil {
			// Persist failed after a successful rotate: still install the
			// new grant as live so it is not thrown away.
			return refreshed, nil
		}
		return refreshed, nil
	}
	if isInvalidGrant(rerr) {
		return nil, fmt.Errorf("snapshot %q's refresh token is no longer valid — run 'claude-sessions account save %s' while logged into that account", name, name)
	}
	return data, nil
}

// rotateParkedSnapshotAccess is the poller's rotate: take the account lock,
// re-read the snapshot (a switch may have just written), and if access is
// still expired call refreshSnapshotCredential. Returns the access token to
// spend. Must not be called from switchAccountLocked — macOS flock is per-fd
// and a nested OpenFile+LOCK_EX deadlocks.
func rotateParkedSnapshotAccess(name string) (string, error) {
	var tok string
	err := withAccountLock(func() error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(snapshotCredentialPath(home, name))
		if err != nil {
			return err
		}
		creds, err := parseOAuthCredentialBlob(data)
		if err != nil {
			return err
		}
		if creds.AccessToken == "" {
			return fmt.Errorf("no access token in credentials")
		}
		exp := msExpiry(creds.ExpiresAt)
		if exp.IsZero() || !usageClockNow().After(exp.Add(usageExpiryClockSkew)) {
			tok = creds.AccessToken
			return nil
		}
		// Re-check under the lock. knownAccountUsage skipped this name
		// against a liveEmail captured at the start of the pass; a
		// switch to this account can finish in between, install the
		// original bytes (rotate failed), and leave this snapshot's
		// refresh token as the live grant. Rotating it here would
		// invalidate the Keychain copy Claude Code is using.
		if emailMatchesLive(snapshotAccountEmail(name), loadAccountEmail()) {
			tok = creds.AccessToken
			return nil
		}
		refreshed, err := refreshSnapshotCredential(home, name)
		if err != nil {
			return err
		}
		tok, err = parseOAuthCredentials(refreshed)
		return err
	})
	return tok, err
}

func jsonRawString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

func oauthScopesFromBlob(raw json.RawMessage) []string {
	if len(raw) != 0 && string(raw) != "null" {
		var scopes []string
		if err := json.Unmarshal(raw, &scopes); err == nil && len(scopes) > 0 {
			return scopes
		}
	}
	return append([]string(nil), oauthDefaultScopes...)
}

func marshalJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
