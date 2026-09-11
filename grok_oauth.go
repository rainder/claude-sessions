package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Grok's public OIDC token endpoint (verified against
// https://auth.x.ai/.well-known/openid-configuration token_endpoint).
const grokTokenURL = "https://auth.x.ai/oauth2/token"

const grokRefreshTimeout = 30 * time.Second

// grokPreferredIssuerPrefix is the live Grok CLI's default issuer key. A
// snapshot whose preferred entry is not this prefix is a customer OIDC login
// and is out of scope for rotate — keep the original bytes.
const grokPreferredIssuerPrefix = "https://auth.x.ai::"

type grokTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// grokOAuthError is a non-200 answer from Grok's token endpoint.
type grokOAuthError struct {
	Status int
	Code   string
}

func (e *grokOAuthError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("grok oauth token: HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("grok oauth token: HTTP %d", e.Status)
}

func isGrokInvalidGrant(err error) bool {
	var e *grokOAuthError
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == "invalid_grant"
}

// grokTokenRefresh is the token-endpoint seam. Production points at
// refreshGrokToken; TestMain defaults it to a panic so a forgotten override
// cannot spend a real refresh token.
var grokTokenRefresh = refreshGrokToken

// refreshGrokToken POSTs a refresh_token grant. Public client (token endpoint
// auth method "none"): client_id in the body, no secret, no Basic auth.
func refreshGrokToken(refreshToken, clientID string) (*grokTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	req, err := http.NewRequest(http.MethodPost, grokTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: grokRefreshTimeout}
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
		return nil, &grokOAuthError{Status: resp.StatusCode, Code: raw.Error}
	}
	var tok grokTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("parse grok token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("grok token response missing access_token")
	}
	return &tok, nil
}

// rotateGrokAuthBytes rotates the preferred xAI entry in a live/snapshot
// auth.json blob. A non-xAI issuer is returned unchanged (customer OIDC is
// out of scope). No file I/O — callers persist.
func rotateGrokAuthBytes(data []byte) ([]byte, error) {
	doc, err := parseGrokAuth(data)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(doc.issuer, grokPreferredIssuerPrefix) {
		return data, nil
	}
	rt := doc.refreshToken()
	cid := doc.oidcClientID()
	if rt == "" || cid == "" {
		return data, nil
	}
	tok, err := grokTokenRefresh(rt, cid)
	if err != nil {
		return nil, err
	}
	return doc.patchTokens(tok)
}

// rotateGrokSnapshotForSwitch is switchGrokAccount's rotate step: always
// attempted for an xAI snapshot, still inside the caller's grok lock, and
// strictly before the rescue copy so a failed rotate cannot leave the host
// mid-switch. invalid_grant refuses. Any other error keeps the original bytes.
func rotateGrokSnapshotForSwitch(name string, data []byte) ([]byte, error) {
	refreshed, rerr := rotateGrokAuthBytes(data)
	if rerr == nil {
		if werr := writeGrokSnapshotAuth(name, refreshed); werr != nil {
			return refreshed, nil
		}
		return refreshed, nil
	}
	if isGrokInvalidGrant(rerr) {
		return nil, fmt.Errorf("snapshot %q's refresh token is no longer valid — run 'claude-sessions account grok save %s' while logged into that account", name, name)
	}
	return data, nil
}
