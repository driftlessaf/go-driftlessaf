/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"encoding/json"
	"errors"
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

	token, err := c.getToken(t.Context())
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
	_, err := c.getToken(t.Context())
	if err != nil {
		t.Fatalf("first getToken() error: %v", err)
	}

	// Second call should return the cached token without hitting the server.
	token, err := c.getToken(t.Context())
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

func TestGetToken_CachedExpiryCappedAtMaxTTL(t *testing.T) {
	// Linear has been observed advertising expires_in values of ~30 days
	// while the token is invalidated server-side much sooner. The client
	// must cap the cache lifetime so getToken() refreshes within
	// maxTokenCacheTTL regardless of what the token endpoint advertises.
	const advertisedSeconds = 30 * 24 * 60 * 60 // 30 days
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "long-lived-token",
			"expires_in":   advertisedSeconds,
		})
	}))
	defer srv.Close()

	c := NewClient("id", "secret").WithTokenURL(srv.URL)
	if _, err := c.getToken(t.Context()); err != nil {
		t.Fatalf("getToken() error: %v", err)
	}

	// The cached expiry should land within maxTokenCacheTTL of now,
	// not 30 days out as the advertised value would suggest. Allow a
	// small slack window for test execution time around the cap.
	gotTTL := time.Until(c.tokenExpiry)
	if gotTTL > maxTokenCacheTTL {
		t.Errorf("cached TTL = %v, want <= maxTokenCacheTTL (%v) — cap not applied", gotTTL, maxTokenCacheTTL)
	}
	if gotTTL < maxTokenCacheTTL-time.Minute {
		t.Errorf("cached TTL = %v, want close to maxTokenCacheTTL (%v) — cap unexpectedly tight", gotTTL, maxTokenCacheTTL)
	}
}

func TestGetToken_CachedExpiryRespectsShortAdvertisedTTL(t *testing.T) {
	// When the advertised TTL is below maxTokenCacheTTL, the cache must
	// honour the smaller value so the client doesn't outlive a token
	// the server already considers expired.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "short-lived-token",
			"expires_in":   600, // 10 minutes
		})
	}))
	defer srv.Close()

	c := NewClient("id", "secret").WithTokenURL(srv.URL)
	if _, err := c.getToken(t.Context()); err != nil {
		t.Fatalf("getToken() error: %v", err)
	}

	gotTTL := time.Until(c.tokenExpiry)
	// 10 minutes minus 30s buffer = 9m30s, ±1s for test timing.
	want := 600*time.Second - 30*time.Second
	if gotTTL > want || gotTTL < want-time.Second {
		t.Errorf("cached TTL = %v, want ~%v (advertised TTL minus 30s buffer)", gotTTL, want)
	}
}

func TestGetToken_APIKey_NeverFetches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("token endpoint should not be called for API key client")
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("my-api-key").WithTokenURL(srv.URL)

	token, err := c.getToken(t.Context())
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

	_, err := c.getToken(t.Context())
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

	_, _ = c.GetViewer(t.Context())

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

	_, _ = c.GetViewer(t.Context())

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

func TestFetchAttachmentContent_RejectsUntrustedURLs(t *testing.T) {
	// Stand up a server that records whether it was contacted; the test
	// expects FetchAttachmentContent to refuse before any request is sent.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write([]byte(`{"state":"ok"}`))
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("secret-token")

	tests := []struct {
		name string
		url  string
	}{
		{"http (non-https) host", srv.URL + "/attachment.json"},
		{"non-Linear https host", "https://example.com/attachment.json"},
		{"plain IP", "https://169.254.169.254/computeMetadata/v1/"},
		{"file scheme", "file:///etc/passwd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.FetchAttachmentContent(t.Context(), tc.url); err == nil {
				t.Errorf("FetchAttachmentContent(%q) returned nil error, want non-nil", tc.url)
			}
		})
	}
	if hits != 0 {
		t.Errorf("test server received %d requests, want 0 (URLs should be rejected before fetch)", hits)
	}
}

// TestSetIssueStateByType_HappyPath proves the lookup-by-type query
// finds the right state on the issue's team and feeds its ID into the
// issueUpdate mutation. Workspace-renamed states ("Done" → "Resolved"
// etc.) survive this lookup because we key off `state.type`, which is
// schema-stable, instead of the per-workspace name.
func TestSetIssueStateByType_HappyPath(t *testing.T) {
	var sawMutationStateID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case contains(req.Query, "team {"):
			// Lookup query — return a team with three states, only one of
			// which has type=canceled. Exercises the type-filter loop.
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"id": "issue-id-1",
						"team": map[string]any{
							"states": map[string]any{
								"nodes": []map[string]any{
									{"id": "state-triage", "type": "triage"},
									{"id": "state-done", "type": "completed"},
									{"id": "state-cancel-1", "type": "canceled"},
								},
							},
						},
					},
				},
			})
		case contains(req.Query, "issueUpdate"):
			// Mutation — capture the stateId variable.
			if v, ok := req.Variables["stateId"].(string); ok {
				sawMutationStateID = v
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issueUpdate": map[string]any{"success": true},
				},
			})
		default:
			t.Errorf("unexpected query: %q", req.Query)
		}
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)
	if err := c.SetIssueStateByType(t.Context(), "issue-id-1", "canceled"); err != nil {
		t.Fatalf("SetIssueStateByType: %v", err)
	}
	if sawMutationStateID != "state-cancel-1" {
		t.Errorf("mutation stateId = %q, want state-cancel-1 (the only canceled-typed state)", sawMutationStateID)
	}
}

// TestSetIssueStateByType_NoMatchingStateNoOps proves the silent-no-op
// behavior on workspaces that don't have a state of the requested type:
// returns nil without firing the mutation. Mostly useful for forward
// compat — if a workspace removes "Canceled" from a team's workflow,
// the PR-closed event handler should not blow up.
func TestSetIssueStateByType_NoMatchingStateNoOps(t *testing.T) {
	var mutationFired atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case contains(req.Query, "team {"):
			// Team has only triage + completed states — no "canceled".
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"id": "issue-id-2",
						"team": map[string]any{
							"states": map[string]any{
								"nodes": []map[string]any{
									{"id": "state-triage", "type": "triage"},
									{"id": "state-done", "type": "completed"},
								},
							},
						},
					},
				},
			})
		case contains(req.Query, "issueUpdate"):
			mutationFired.Store(true)
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"issueUpdate": map[string]any{"success": true}},
			})
		}
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)
	if err := c.SetIssueStateByType(t.Context(), "issue-id-2", "canceled"); err != nil {
		t.Fatalf("SetIssueStateByType: should silently no-op when team has no canceled state, got error: %v", err)
	}
	if mutationFired.Load() {
		t.Error("issueUpdate mutation must NOT fire when no state of the requested type exists")
	}
}

// TestSetIssueStateByType_PreferredNameDisambiguates is the regression
// test for the observed "PR closed but issue went to Duplicate not
// Canceled" bug. Linear's schema maps both the "Cancelled"/"Canceled"
// AND the "Duplicate" workflow states to type=canceled — so a
// type-only lookup picks whichever the team's `states.nodes` array
// returns first, which can be Duplicate.
//
// Callers pass preferredNames to disambiguate. The lookup must pick
// the first state whose type matches AND whose name (case-insensitive)
// appears in preferredNames, ignoring earlier type-matching entries
// that don't match by name.
func TestSetIssueStateByType_PreferredNameDisambiguates(t *testing.T) {
	var sawMutationStateID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case contains(req.Query, "team {"):
			// Mirror the Chainguard team config: Duplicate appears BEFORE
			// Canceled in the team's state list, both with type=canceled.
			// Without preferredNames, the type-only lookup would pick
			// state-duplicate (the bug). With preferredNames=["Canceled"],
			// it must pick state-canceled.
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"id": "issue-id-3",
						"team": map[string]any{
							"states": map[string]any{
								"nodes": []map[string]any{
									{"id": "state-triage", "name": "Triage", "type": "triage"},
									{"id": "state-done", "name": "Done", "type": "completed"},
									{"id": "state-duplicate", "name": "Duplicate", "type": "canceled"},
									{"id": "state-canceled", "name": "Canceled", "type": "canceled"},
								},
							},
						},
					},
				},
			})
		case contains(req.Query, "issueUpdate"):
			if v, ok := req.Variables["stateId"].(string); ok {
				sawMutationStateID = v
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issueUpdate": map[string]any{"success": true},
				},
			})
		default:
			t.Errorf("unexpected query: %q", req.Query)
		}
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)
	if err := c.SetIssueStateByType(t.Context(), "issue-id-3", "canceled", "Canceled", "Cancelled"); err != nil {
		t.Fatalf("SetIssueStateByType: %v", err)
	}
	if sawMutationStateID != "state-canceled" {
		t.Errorf("mutation stateId = %q, want state-canceled (preferredNames must override first-by-type)", sawMutationStateID)
	}
}

// TestSetIssueStateByType_PreferredNameFallsBack proves that when no
// preferred name matches, the lookup falls back to the first state by
// type alone — preserving the workspace-rename-survival guarantee for
// teams that use a non-standard cancel name (e.g. "Won't Do").
func TestSetIssueStateByType_PreferredNameFallsBack(t *testing.T) {
	var sawMutationStateID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case contains(req.Query, "team {"):
			// Team uses "Won't Do" instead of "Canceled". Bot still asks
			// for preferred names ("Canceled","Cancelled") but neither
			// matches — fall back to the first canceled-typed state.
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"id": "issue-id-4",
						"team": map[string]any{
							"states": map[string]any{
								"nodes": []map[string]any{
									{"id": "state-wontdo", "name": "Won't Do", "type": "canceled"},
								},
							},
						},
					},
				},
			})
		case contains(req.Query, "issueUpdate"):
			if v, ok := req.Variables["stateId"].(string); ok {
				sawMutationStateID = v
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issueUpdate": map[string]any{"success": true},
				},
			})
		}
	}))
	defer srv.Close()

	c := NewClientWithAPIKey("lin_api_test").WithEndpoint(srv.URL)
	if err := c.SetIssueStateByType(t.Context(), "issue-id-4", "canceled", "Canceled", "Cancelled"); err != nil {
		t.Fatalf("SetIssueStateByType: %v", err)
	}
	if sawMutationStateID != "state-wontdo" {
		t.Errorf("mutation stateId = %q, want state-wontdo (fallback to first-by-type when no name preference matches)", sawMutationStateID)
	}
}

// contains is a tiny substring helper so tests don't pull in strings just
// to do a single Contains check.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
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

func TestGetDocument_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Variables["id"] != "abc123" {
			t.Errorf("variables.id = %v, want abc123", req.Variables["id"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"document": map[string]any{
					"id":      "doc-uuid-1",
					"slugId":  "abc123",
					"title":   "Design",
					"content": "# heading\n\nbody",
					"url":     "https://linear.app/test/document/abc123",
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	doc, err := client.GetDocument(t.Context(), "abc123")
	if err != nil {
		t.Fatalf("GetDocument() error: %v", err)
	}
	if doc.ID != "doc-uuid-1" {
		t.Errorf("ID = %v, want doc-uuid-1", doc.ID)
	}
	if doc.Slug != "abc123" {
		t.Errorf("Slug = %v, want abc123", doc.Slug)
	}
	if doc.Content != "# heading\n\nbody" {
		t.Errorf("Content = %v, want # heading\\n\\nbody", doc.Content)
	}
	if doc.URL != "https://linear.app/test/document/abc123" {
		t.Errorf("URL = %v", doc.URL)
	}
}

func TestGetDocument_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"document": nil,
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	_, err := client.GetDocument(t.Context(), "missing")
	if !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("GetDocument() error = %v, want ErrDocumentNotFound", err)
	}
}
