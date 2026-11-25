/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"google.golang.org/genai"
)

func TestCacheErrorClassifiers(t *testing.T) {
	t.Parallel()

	const (
		expiredMsg = "Cache content cachedContents/abc is expired."
		belowMsg   = "The minimum token count to start explicit caching is 1024."
	)
	tests := []struct {
		name         string
		err          error
		wantExpired  bool
		wantBelowMin bool
	}{
		{name: "nil"},
		{name: "expired", err: genai.APIError{Code: 400, Message: expiredMsg, Status: "INVALID_ARGUMENT"}, wantExpired: true},
		{name: "expired wrapped", err: fmt.Errorf("send: %w", genai.APIError{Code: 400, Message: expiredMsg}), wantExpired: true},
		{name: "expired joined", err: errors.Join(errors.New("other"), genai.APIError{Code: 400, Message: expiredMsg}), wantExpired: true},
		{name: "below minimum", err: genai.APIError{Code: 400, Message: belowMsg, Status: "INVALID_ARGUMENT"}, wantBelowMin: true},
		{name: "other 400", err: genai.APIError{Code: 400, Message: "invalid argument"}},
		{name: "expired text on a 500", err: genai.APIError{Code: 500, Message: expiredMsg}},
		{name: "below minimum text on a 429", err: genai.APIError{Code: 429, Message: belowMsg}},
		{name: "expired text without a code", err: errors.New(expiredMsg)},
		{name: "below minimum text without a code", err: errors.New(belowMsg)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isCacheExpiredError(tt.err); got != tt.wantExpired {
				t.Errorf("isCacheExpiredError: got = %v, want = %v", got, tt.wantExpired)
			}
			if got := isCacheBelowMinimumError(tt.err); got != tt.wantBelowMin {
				t.Errorf("isCacheBelowMinimumError: got = %v, want = %v", got, tt.wantBelowMin)
			}
		})
	}
}

// TestGetOrCreateCacheSkipsBelowMinimumForOneTTL checks that a create the
// provider rejects as below the model minimum is not repeated until one cache
// TTL has passed, and that the rejection does not read as a failure.
func TestGetOrCreateCacheSkipsBelowMinimumForOneTTL(t *testing.T) {
	t.Parallel()

	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		creates.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"The minimum token count to start explicit caching is 1024.","status":"INVALID_ARGUMENT"}}`)
	}))
	t.Cleanup(srv.Close)
	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		Backend:     genai.BackendGeminiAPI,
		APIKey:      "test",
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	iface, err := New[*testBindable, *testResponse](client, prompt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := iface.(*executor[*testBindable, *testResponse])
	system := genai.NewContentFromText("system", genai.RoleUser)

	for range 3 {
		if _, _, err := e.getOrCreateCache(t.Context(), system, nil); !errors.Is(err, errCacheCreationHeld) {
			t.Fatalf("getOrCreateCache: got err = %v, want = %v", err, errCacheCreationHeld)
		}
	}
	if got, want := creates.Load(), int32(1); got != want {
		t.Fatalf("create calls inside the skip window: got = %d, want = %d", got, want)
	}
	e.cacheMu.Lock()
	until := e.cacheRetryAfter
	e.cacheMu.Unlock()
	if remaining := time.Until(until); remaining < e.cacheTTL-time.Minute || remaining > e.cacheTTL {
		t.Errorf("skip window: got = %v remaining, want about one cache TTL (%v)", remaining, e.cacheTTL)
	}

	e.cacheMu.Lock()
	e.cacheRetryAfter = time.Now().Add(-time.Second)
	e.cacheMu.Unlock()
	if _, _, err := e.getOrCreateCache(t.Context(), system, nil); !errors.Is(err, errCacheCreationHeld) {
		t.Fatalf("getOrCreateCache after the window: got err = %v, want = %v", err, errCacheCreationHeld)
	}
	if got, want := creates.Load(), int32(2); got != want {
		t.Errorf("create calls after the skip window: got = %d, want = %d", got, want)
	}
}
