package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func stubOAuthRefresh(t *testing.T, fn func(string, []string) (*oauthTokenResponse, error)) {
	t.Helper()
	prev := oauthTokenRefresh
	oauthTokenRefresh = fn
	t.Cleanup(func() { oauthTokenRefresh = prev })
}

func cannedTokenResponse() *oauthTokenResponse {
	return &oauthTokenResponse{
		AccessToken:           "new-access",
		RefreshToken:          "new-refresh",
		ExpiresIn:             3600,
		RefreshTokenExpiresIn: 86400,
		Scope:                 "user:profile user:inference",
	}
}

func decodeOAuthBlob(t *testing.T, data []byte) (outer, inner map[string]json.RawMessage) {
	t.Helper()
	if err := json.Unmarshal(data, &outer); err != nil {
		t.Fatalf("outer: %v", err)
	}
	if err := json.Unmarshal(outer["claudeAiOauth"], &inner); err != nil {
		t.Fatalf("inner: %v", err)
	}
	return outer, inner
}

func jsonFieldString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("string field: %v (%s)", err, raw)
	}
	return s
}

func jsonFieldInt64(t *testing.T, raw json.RawMessage) int64 {
	t.Helper()
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("int field: %v (%s)", err, raw)
	}
	return n
}

func TestRefreshOAuthCredentialPatchesTokensAndExpiry(t *testing.T) {
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		return cannedTokenResponse(), nil
	})
	blob := []byte(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":1}}`)
	before := time.Now()
	got, err := refreshOAuthCredential(blob)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	_, inner := decodeOAuthBlob(t, got)
	if jsonFieldString(t, inner["accessToken"]) != "new-access" {
		t.Fatalf("accessToken = %s", inner["accessToken"])
	}
	if jsonFieldString(t, inner["refreshToken"]) != "new-refresh" {
		t.Fatalf("refreshToken = %s", inner["refreshToken"])
	}
	gotExp := jsonFieldInt64(t, inner["expiresAt"])
	wantLo := before.Add(3600 * time.Second).UnixMilli()
	wantHi := time.Now().Add(3600 * time.Second).UnixMilli()
	if gotExp < wantLo || gotExp > wantHi {
		t.Fatalf("expiresAt = %d, want in [%d, %d]", gotExp, wantLo, wantHi)
	}
}

func TestRefreshOAuthCredentialPreservesUnknownKeys(t *testing.T) {
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		return cannedTokenResponse(), nil
	})
	blob := []byte(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":1,"refreshTokenExpiresAt":2,"scopes":["user:profile"],"subscriptionType":"pro","rateLimitTier":"default_tier","mysteryInner":"keep-inner"},"mcpOAuth":{"someServer":{"accessToken":"mcp-tok"}},"mysteryOuter":42}`)
	got, err := refreshOAuthCredential(blob)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	outer, inner := decodeOAuthBlob(t, got)
	if _, ok := inner["clientId"]; ok {
		t.Fatalf("inserted clientId = %s", inner["clientId"])
	}
	if jsonFieldString(t, inner["subscriptionType"]) != "pro" {
		t.Fatalf("subscriptionType = %s", inner["subscriptionType"])
	}
	if jsonFieldString(t, inner["rateLimitTier"]) != "default_tier" {
		t.Fatalf("rateLimitTier = %s", inner["rateLimitTier"])
	}
	if jsonFieldString(t, inner["mysteryInner"]) != "keep-inner" {
		t.Fatalf("mysteryInner = %s", inner["mysteryInner"])
	}
	if !strings.Contains(string(outer["mcpOAuth"]), `"someServer"`) ||
		!strings.Contains(string(outer["mcpOAuth"]), `"mcp-tok"`) {
		t.Fatalf("mcpOAuth = %s, want the original object kept", outer["mcpOAuth"])
	}
	if jsonFieldInt64(t, outer["mysteryOuter"]) != 42 {
		t.Fatalf("mysteryOuter = %s", outer["mysteryOuter"])
	}
}

func TestRefreshOAuthCredentialDoesNotInsertClientID(t *testing.T) {
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		return cannedTokenResponse(), nil
	})
	blob := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"ref"}}`)
	got, err := refreshOAuthCredential(blob)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	_, inner := decodeOAuthBlob(t, got)
	if _, ok := inner["clientId"]; ok {
		t.Fatalf("inserted clientId = %s", inner["clientId"])
	}
}

func TestRefreshOAuthCredentialKeepsOldRefreshTokenWhenResponseOmitsIt(t *testing.T) {
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		return &oauthTokenResponse{AccessToken: "new-access", ExpiresIn: 3600}, nil
	})
	blob := []byte(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh"}}`)
	got, err := refreshOAuthCredential(blob)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	_, inner := decodeOAuthBlob(t, got)
	if jsonFieldString(t, inner["refreshToken"]) != "old-refresh" {
		t.Fatalf("refreshToken = %s, want the old one kept", inner["refreshToken"])
	}
}

func TestRefreshOAuthCredentialKeepsOldRefreshExpiryWhenResponseOmitsIt(t *testing.T) {
	stubOAuthRefresh(t, func(string, []string) (*oauthTokenResponse, error) {
		return &oauthTokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}, nil
	})
	blob := []byte(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","refreshTokenExpiresAt":999}}`)
	got, err := refreshOAuthCredential(blob)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	_, inner := decodeOAuthBlob(t, got)
	if jsonFieldInt64(t, inner["refreshTokenExpiresAt"]) != 999 {
		t.Fatalf("refreshTokenExpiresAt = %s, want 999 kept", inner["refreshTokenExpiresAt"])
	}
}

func TestRefreshOAuthCredentialUsesBlobScopes(t *testing.T) {
	var gotScopes []string
	stubOAuthRefresh(t, func(_ string, scopes []string) (*oauthTokenResponse, error) {
		gotScopes = append([]string(nil), scopes...)
		return &oauthTokenResponse{AccessToken: "new-access", ExpiresIn: 3600}, nil
	})
	want := []string{"user:profile", "user:sessions:claude_code"}
	blob, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "old-access",
			"refreshToken": "old-refresh",
			"scopes":       want,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refreshOAuthCredential(blob); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(gotScopes, want) {
		t.Fatalf("scopes = %v, want blob scopes %v", gotScopes, want)
	}
}

func TestRefreshOAuthCredentialDefaultScopesWhenMissingOrEmpty(t *testing.T) {
	for _, name := range []string{"missing", "empty", "null"} {
		t.Run(name, func(t *testing.T) {
			var gotScopes []string
			stubOAuthRefresh(t, func(_ string, scopes []string) (*oauthTokenResponse, error) {
				gotScopes = append([]string(nil), scopes...)
				return &oauthTokenResponse{AccessToken: "new-access", ExpiresIn: 3600}, nil
			})
			inner := map[string]any{"accessToken": "old-access", "refreshToken": "old-refresh"}
			switch name {
			case "empty":
				inner["scopes"] = []string{}
			case "null":
				inner["scopes"] = nil
			}
			blob, err := json.Marshal(map[string]any{"claudeAiOauth": inner})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := refreshOAuthCredential(blob); err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(gotScopes, oauthDefaultScopes) {
				t.Fatalf("scopes = %v, want default %v", gotScopes, oauthDefaultScopes)
			}
		})
	}
}

func TestIsInvalidGrant(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 invalid_grant", &oauthRefreshError{Status: 400, Code: "invalid_grant"}, true},
		{"401 invalid_grant", &oauthRefreshError{Status: 401, Code: "invalid_grant"}, true},
		{"invalid_grant code", &oauthRefreshError{Status: 403, Code: "invalid_grant"}, true},
		{"wrapped 400", fmt.Errorf("refreshing: %w", &oauthRefreshError{Status: 400, Code: "invalid_grant"}), true},
		{"bare 400", &oauthRefreshError{Status: 400}, false},
		{"400 invalid_scope", &oauthRefreshError{Status: 400, Code: "invalid_scope"}, false},
		{"401 without code", &oauthRefreshError{Status: 401}, false},
		{"500", &oauthRefreshError{Status: 500}, false},
		{"plain error", errors.New("dial tcp"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isInvalidGrant(c.err); got != c.want {
				t.Fatalf("isInvalidGrant(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
