/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	sdkoption "github.com/anthropics/anthropic-sdk-go/option"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

func TestVertexAnthropicMessageServiceDisablesSDKRetries(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()

	messages, err := newVertexAnthropicMessageService(
		t.Context(),
		"test-project",
		"us-central1",
		&google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})},
		func(context.Context, string, string, *google.Credentials) sdkoption.RequestOption {
			return sdkoption.WithMiddleware(func(r *http.Request, next sdkoption.MiddlewareNext) (*http.Response, error) {
				u, _ := url.Parse(srv.URL)
				r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
				r.Header.Set("X-Api-Key", "test")
				return next(r)
			})
		},
	)
	if err != nil {
		t.Fatalf("newVertexAnthropicMessageService() = %v", err)
	}

	_, err = messages.New(t.Context(), anthropic.MessageNewParams{
		Model:     "claude-test",
		MaxTokens: 16,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
		},
	})
	if err == nil {
		t.Fatal("Messages.New() error = nil, want 429 error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (SDK retries disabled)", got)
	}
}
