/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewClient_OAuth(t *testing.T) {
	c := NewClient("client-id", "client-secret")

	if c.clientID != "client-id" {
		t.Errorf("clientID = %q, want %q", c.clientID, "client-id")
	}
	if c.clientSecret != "client-secret" {
		t.Errorf("clientSecret = %q, want %q", c.clientSecret, "client-secret")
	}
	if c.isAPIKey {
		t.Error("isAPIKey should be false for OAuth client")
	}
	if c.endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want %q", c.endpoint, DefaultEndpoint)
	}
	if c.tokenURL != DefaultTokenURL {
		t.Errorf("tokenURL = %q, want %q", c.tokenURL, DefaultTokenURL)
	}
	if len(c.scopes) != 2 || c.scopes[0] != ScopeRead || c.scopes[1] != ScopeWrite {
		t.Errorf("scopes = %v, want [%s, %s]", c.scopes, ScopeRead, ScopeWrite)
	}
}

func TestNewClientWithAPIKey(t *testing.T) {
	c := NewClientWithAPIKey("lin_api_key123")

	if !c.isAPIKey {
		t.Error("isAPIKey should be true for API key client")
	}
	if c.token != "lin_api_key123" {
		t.Errorf("token = %q, want %q", c.token, "lin_api_key123")
	}
	if c.tokenExpiry.Before(time.Now().Add(100 * 365 * 24 * time.Hour)) {
		t.Error("API key token expiry should be far in the future")
	}
}

func TestWithScopes(t *testing.T) {
	c := NewClient("id", "secret").
		WithScopes(ScopeRead, ScopeIssuesCreate, ScopeCommentsCreate)

	if len(c.scopes) != 3 {
		t.Fatalf("scopes length = %d, want 3", len(c.scopes))
	}
	if c.scopes[0] != ScopeRead {
		t.Errorf("scopes[0] = %q, want %q", c.scopes[0], ScopeRead)
	}
	if c.scopes[1] != ScopeIssuesCreate {
		t.Errorf("scopes[1] = %q, want %q", c.scopes[1], ScopeIssuesCreate)
	}
	if c.scopes[2] != ScopeCommentsCreate {
		t.Errorf("scopes[2] = %q, want %q", c.scopes[2], ScopeCommentsCreate)
	}
}

func TestGetToken_OAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("token request method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
		}

		if err := r.ParseForm(); err != nil {
			t.Fatalf("parsing form: %v", err)
		}
		if got := r.FormValue("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", got)
		}
		if got := r.FormValue("client_id"); got != "test-client-id" {
			t.Errorf("client_id = %q, want test-client-id", got)
		}
		if got := r.FormValue("client_secret"); got != "test-client-secret" {
			t.Errorf("client_secret = %q, want test-client-secret", got)
		}
		if got := r.FormValue("scope"); got != "read,write" {
			t.Errorf("scope = %q, want read,write", got)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "oauth-token-abc",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	c := NewClient("test-client-id", "test-client-secret").
		WithTokenURL(srv.URL)

	token, err := c.getToken(context.Background())
	if err != nil {
		t.Fatalf("getToken() error: %v", err)
	}
	if token != "oauth-token-abc" {
		t.Errorf("token = %q, want %q", token, "oauth-token-abc")
	}
}

func TestGetToken_Caching(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "cached-token",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	c := NewClient("id", "secret").WithTokenURL(srv.URL)

	// First call fetches a new token.
	_, err := c.getToken(context.Background())
	if err != nil {
		t.Fatalf("first getToken() error: %v", err)
	}

	// Second call should return the cached token without hitting the server.
	token, err := c.getToken(context.Background())
	if err != nil {
		t.Fatalf("second getToken() error: %v", err)
	}
	if token != "cached-token" {
		t.Errorf("token = %q, want %q", token, "cached-token")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1", got)
	}
}

func TestGetToken_APIKey_NeverFetches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("token endpoint should not be called for API key client")
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("my-api-key").WithTokenURL(srv.URL)

	token, err := c.getToken(context.Background())
	if err != nil {
		t.Fatalf("getToken() error: %v", err)
	}
	if token != "my-api-key" {
		t.Errorf("token = %q, want %q", token, "my-api-key")
	}
}

func TestGetToken_OAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	c := NewClient("bad-id", "bad-secret").WithTokenURL(srv.URL)

	_, err := c.getToken(context.Background())
	if err == nil {
		t.Fatal("expected error for failed token exchange")
	}
}

func TestAuthHeader_APIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"viewer": map[string]any{"id": "bot-1", "name": "Bot"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("lin_api_test123").
		WithEndpoint(srv.URL)

	_, _ = c.GetViewer(context.Background())

	if gotAuth != "lin_api_test123" {
		t.Errorf("Authorization header = %q, want raw API key %q", gotAuth, "lin_api_test123")
	}
}

func TestAuthHeader_OAuth_Bearer(t *testing.T) {
	var gotAuth string

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "oauth-access-token",
			"expires_in":   3600,
		})
	}))
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"viewer": map[string]any{"id": "bot-1", "name": "Bot"},
			},
		})
	}))
	defer apiSrv.Close()

	c := NewClient("id", "secret").
		WithTokenURL(tokenSrv.URL).
		WithEndpoint(apiSrv.URL)

	_, _ = c.GetViewer(context.Background())

	if gotAuth != "Bearer oauth-access-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer oauth-access-token")
	}
}

func TestSensitiveHeaders_Filtered(t *testing.T) {
	tests := []struct {
		key      string
		filtered bool
	}{
		{"Authorization", true},
		{"authorization", true},
		{"Cookie", true},
		{"Host", true},
		{"Proxy-Authorization", true},
		{"X-Amz-Content-Sha256", false},
		{"Content-Type", false},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			_, got := sensitiveHeaders[http.CanonicalHeaderKey(tc.key)]
			if got != tc.filtered {
				t.Errorf("sensitiveHeaders[%q] = %v, want %v", tc.key, got, tc.filtered)
			}
		})
	}
}

func TestFetchAttachmentContent_NoAuthToUntrustedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("Authorization header should not be sent to non-Linear hosts, got %q", auth)
		}
		w.Write([]byte(`{"state":"ok"}`))
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("secret-token")

	data, err := c.FetchAttachmentContent(context.Background(), srv.URL+"/attachment.json")
	if err != nil {
		t.Fatalf("FetchAttachmentContent() error: %v", err)
	}
	if string(data) != `{"state":"ok"}` {
		t.Errorf("data = %q, want %q", data, `{"state":"ok"}`)
	}
}

func TestIsLinearHost(t *testing.T) {
	tests := []struct {
		host    string
		trusted bool
	}{
		{"linear.app", true},
		{"api.linear.app", true},
		{"uploads.linear.app", true},
		{"cdn.uploads.linear.app", true},
		{"evil.com", false},
		{"notlinear.app", false},
		{"linear.app.evil.com", false},
		{"api.linear.app.evil.com", false},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := isLinearHost(tc.host); got != tc.trusted {
				t.Errorf("isLinearHost(%q) = %v, want %v", tc.host, got, tc.trusted)
			}
		})
	}
}
