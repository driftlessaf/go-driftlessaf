/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"google.golang.org/genai"
)

// TestRequestTimeoutBoundsEachContinuationRequest checks the SDK behavior the
// long-decode bound relies on: the adapter's default request timeout applies
// to each request of an automatically continued generation, not to the whole
// generation.
func TestRequestTimeoutBoundsEachContinuationRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// segmentDelays is how long the fake server takes to answer each
		// request. The first answer asks for a continuation.
		segmentDelays []time.Duration
		wantErr       error
		wantRequests  int
	}{{
		// Each segment answers within the bound, and the two together take
		// longer than it.
		name:          "each segment within the bound",
		segmentDelays: []time.Duration{longDecodeRequestTimeout * 3 / 4, longDecodeRequestTimeout * 3 / 4},
		wantRequests:  2,
	}, {
		// Shows that the bound is live on this path, so the row above
		// passes because it applies per request and not because it is
		// missing.
		name:          "one segment past the bound",
		segmentDelays: []time.Duration{longDecodeRequestTimeout + time.Minute, 0},
		wantErr:       context.DeadlineExceeded,
		wantRequests:  1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					defer r.Body.Close()
					requests++
					select {
					case <-time.After(tc.segmentDelays[requests-1]):
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"second"}]},"finishReason":"STOP"}]}`
					if requests == 1 {
						body = `{"candidates":[{"content":{"role":"model","parts":[{"text":"first "}]},"finishReason":"CONTINUATION","continuationToken":"Y29udGludWU="}]}`
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}

				route := routedTestRoute(
					modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "gemini-4-argon"},
					modelrouter.ProtocolGoogleGenAI,
					"publishers/google/models/gemini-4-argon",
				)
				plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				// The factory keeps the adapter's HTTP options, timeout
				// included, and sends the requests to the fake transport. The
				// Gemini API backend needs no Google credentials.
				adapter, err := newVertexGoogleGenAIAdapterWithRequestTimeout("test-project", "us-central1", 0, func(ctx context.Context, config *genai.ClientConfig) (*genai.Client, error) {
					opts := config.HTTPOptions
					opts.BaseURL = "https://genai.invalid"
					return genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendGeminiAPI, APIKey: "test", HTTPClient: client, HTTPOptions: opts})
				})
				if err != nil {
					t.Fatalf("newVertexGoogleGenAIAdapterWithRequestTimeout: %v", err)
				}
				binding, err := adapter(t.Context(), plan)
				if err != nil {
					t.Fatalf("adapter: %v", err)
				}

				resp, err := binding.client.Models.GenerateContent(t.Context(), "gemini-4-argon", genai.Text("translate"), nil)
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Errorf("GenerateContent: got error %v, want %v", err, tc.wantErr)
					}
				} else if err != nil {
					t.Errorf("GenerateContent: got error %v, want the continued generation to finish", err)
				} else if got := resp.Text(); got != "first second" {
					t.Errorf("text: got = %q, want = %q", got, "first second")
				}
				if requests != tc.wantRequests {
					t.Errorf("requests: got = %d, want = %d", requests, tc.wantRequests)
				}
			})
		})
	}
}
