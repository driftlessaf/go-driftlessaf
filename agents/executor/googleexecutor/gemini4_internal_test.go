/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

// TestNewOutputTokenLimit checks that New holds max_output_tokens to the
// configured model's limit once every option is applied, so option order
// does not matter.
func TestNewOutputTokenLimit(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	tests := []struct {
		name    string
		opts    []Option[*testBindable, *testResponse]
		wantErr bool
	}{{
		name: "gemini 3.8 at the default limit",
		opts: []Option[*testBindable, *testResponse]{WithMaxOutputTokens[*testBindable, *testResponse](65536)},
	}, {
		name:    "gemini 3.8 above the default limit",
		opts:    []Option[*testBindable, *testResponse]{WithMaxOutputTokens[*testBindable, *testResponse](65537)},
		wantErr: true,
	}, {
		name: "gemini 4 long decode above one request",
		opts: []Option[*testBindable, *testResponse]{
			WithModel[*testBindable, *testResponse]("gemini-4-argon"),
			WithMaxOutputTokens[*testBindable, *testResponse](200000),
		},
	}, {
		name: "gemini 4 limit holds whatever the option order",
		opts: []Option[*testBindable, *testResponse]{
			WithMaxOutputTokens[*testBindable, *testResponse](1048576),
			WithModel[*testBindable, *testResponse]("gemini-4-argon"),
		},
	}, {
		name: "gemini 4 above its context window",
		opts: []Option[*testBindable, *testResponse]{
			WithModel[*testBindable, *testResponse]("gemini-4-argon"),
			WithMaxOutputTokens[*testBindable, *testResponse](1048577),
		},
		wantErr: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New[*testBindable, *testResponse](nil, prompt, tt.opts...); (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestNewThinkingBudget checks that New refuses WithThinking on a model that
// rejects thinkingBudget, instead of failing on the first request.
func TestNewThinkingBudget(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	tests := []struct {
		model   string
		wantErr bool
	}{
		{model: "gemini-3.5-flash"},
		{model: "gemini-2.5-flash"},
		{model: "gemini-4-argon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			_, err := New[*testBindable, *testResponse](nil, prompt,
				WithModel[*testBindable, *testResponse](tt.model),
				WithThinking[*testBindable, *testResponse](2048))
			if (err != nil) != tt.wantErr {
				t.Errorf("New(%q, WithThinking) error = %v, wantErr %v", tt.model, err, tt.wantErr)
			}
		})
	}
}

// TestGenerationConfigAutomaticContinuation checks that a long-decode model
// asks the SDK to follow continuation tokens and other models leave the SDK
// default (unset).
func TestGenerationConfigAutomaticContinuation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  *bool
	}{
		{model: "gemini-4-argon", want: new(true)},
		{model: "gemini-3.8-flash"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			e := &executor[*testBindable, *testResponse]{capabilityModel: tt.model}
			if diff := cmp.Diff(tt.want, e.generationConfig().AutomaticContinuation); diff != "" {
				t.Errorf("AutomaticContinuation for %q (-want, +got):\n%s", tt.model, diff)
			}
		})
	}
}

// TestGetOrCreateCacheHoldsAfterRejection checks that a rejected create holds
// later creates, from concurrent callers too, and that a create is tried
// again once the hold ends.
func TestGetOrCreateCacheHoldsAfterRejection(t *testing.T) {
	t.Parallel()

	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		creates.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"message":"Permission denied on resource.","status":"PERMISSION_DENIED"}}`)
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

	if _, _, err := e.getOrCreateCache(t.Context(), system, nil); err == nil || errors.Is(err, errCacheCreationHeld) {
		t.Fatalf("first getOrCreateCache: got err = %v, want the provider rejection", err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, _, err := e.getOrCreateCache(t.Context(), system, nil); !errors.Is(err, errCacheCreationHeld) {
				t.Errorf("getOrCreateCache during the hold: got err = %v, want = %v", err, errCacheCreationHeld)
			}
		})
	}
	wg.Wait()
	if got, want := creates.Load(), int32(1); got != want {
		t.Fatalf("cachedContents create calls during the hold: got = %d, want = %d", got, want)
	}

	e.cacheMu.Lock()
	e.cacheRetryAfter = time.Now().Add(-time.Second)
	e.cacheMu.Unlock()
	if _, _, err := e.getOrCreateCache(t.Context(), system, nil); err == nil || errors.Is(err, errCacheCreationHeld) {
		t.Fatalf("getOrCreateCache after the hold: got err = %v, want the provider rejection", err)
	}
	if got, want := creates.Load(), int32(2); got != want {
		t.Errorf("cachedContents create calls after the hold: got = %d, want = %d", got, want)
	}
}
