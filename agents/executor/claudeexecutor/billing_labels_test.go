/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
)

func TestVertexBillingLabelsHeader(t *testing.T) {
	many := map[string]string{"service_name": "example-service", "team": "example-team", "product": "example-product"}
	manyWant := map[string]string{"service_name": "example-service", "team": "example-team", "product": "example-product"}
	for i := range 40 {
		k := fmt.Sprintf("extra_%02d", i)
		many[k] = "x"
		if i < 29 {
			manyWant[k] = "x"
		}
	}
	tests := []struct {
		name     string
		provider claudeexecutor.Provider
		routed   *agenttrace.Attribution
		labels   map[string]string
		want     map[string]string
	}{{
		name:     "vertex sends valid labels",
		provider: claudeexecutor.ProviderVertex,
		labels:   map[string]string{"service_name": "example-service", "team": "example-team", "product": "example-product"},
		want:     map[string]string{"service_name": "example-service", "team": "example-team", "product": "example-product"},
	}, {
		name:     "vertex drops invalid labels",
		provider: claudeexecutor.ProviderVertex,
		labels:   map[string]string{"service_name": "example-service", "team": "Example", "Product": "x"},
		want:     map[string]string{"service_name": "example-service"},
	}, {
		name:     "vertex omits placeholder owners",
		provider: claudeexecutor.ProviderVertex,
		labels:   map[string]string{"service_name": "example-service", "team": "unknown", "product": ""},
		want:     map[string]string{"service_name": "example-service"},
	}, {
		name:     "vertex keeps attribution labels past the limit",
		provider: claudeexecutor.ProviderVertex,
		labels:   many,
		want:     manyWant,
	}, {
		name:     "anthropic sends no labels",
		provider: claudeexecutor.ProviderAnthropic,
		labels:   map[string]string{"service_name": "example-service"},
	}, {
		name:   "routed vertex sends valid labels",
		routed: &agenttrace.Attribution{ProviderName: "gcp.vertex_ai", System: "gcp.vertex_ai", LogicalModel: "claude-sonnet-4-6", Protocol: "anthropic-messages"},
		labels: map[string]string{"service_name": "example-service", "team": "example-team"},
		want:   map[string]string{"service_name": "example-service", "team": "example-team"},
	}, {
		name:   "routed bedrock sends no labels",
		routed: &agenttrace.Attribution{ProviderName: "aws.bedrock", System: "aws.bedrock", LogicalModel: "claude-sonnet-4-6", Protocol: "anthropic-messages"},
		labels: map[string]string{"service_name": "example-service"},
	}, {
		name:   "routed anthropic sends no labels",
		routed: &agenttrace.Attribution{ProviderName: "anthropic", System: "anthropic", LogicalModel: "claude-sonnet-4-6", Protocol: "anthropic-messages"},
		labels: map[string]string{"service_name": "example-service"},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var headers []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				headers = append(headers, r.Header.Get("X-Vertex-AI-Labels"))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sseBody(t, submitCallTurn(t, "msg_01", "toolu_s1", submitInput("done"))))
			}))
			t.Cleanup(srv.Close)

			prompt, err := promptbuilder.NewPrompt("go")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
			opts := []claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithResourceLabels[errCapRequest, errCapResponse](tc.labels),
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
			}
			if tc.routed != nil {
				opts = append(opts,
					claudeexecutor.WithRoutedModel[errCapRequest, errCapResponse]("claude-sonnet-4-6", tc.routed.LogicalModel),
					claudeexecutor.WithAttribution[errCapRequest, errCapResponse](*tc.routed))
			} else {
				opts = append(opts, claudeexecutor.WithProvider[errCapRequest, errCapResponse](tc.provider))
			}
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := exec.Execute(t.Context(), errCapRequest{}, nil); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(headers) == 0 {
				t.Fatal("server saw no request")
			}
			for _, h := range headers {
				if tc.want == nil {
					if h != "" {
						t.Errorf("X-Vertex-AI-Labels: got = %q, want = absent", h)
					}
					continue
				}
				raw, err := base64.StdEncoding.DecodeString(h)
				if err != nil {
					t.Fatalf("decoding X-Vertex-AI-Labels %q: %v", h, err)
				}
				var got map[string]string
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("parsing X-Vertex-AI-Labels %q: %v", raw, err)
				}
				// Environment defaults can add labels the test does not set;
				// every label the test set must match want exactly.
				for k := range got {
					if _, ok := tc.labels[k]; !ok {
						delete(got, k)
					}
				}
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("X-Vertex-AI-Labels (-want +got):\n%s", diff)
				}
			}
		})
	}
}
