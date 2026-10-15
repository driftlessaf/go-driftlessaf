/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/genai"
)

// TestNewWithTargetSendsTargetMaxTokens checks that a judge built from a
// target sends the target's output cap, and 8,192 when the target sets none.
func TestNewWithTargetSendsTargetMaxTokens(t *testing.T) {
	t.Parallel()
	for protocol, model := range map[modelrouter.Protocol]string{
		modelrouter.ProtocolAnthropicMessages: "claude-sonnet-4-6",
		modelrouter.ProtocolGoogleGenAI:       "gemini-2.5-flash",
	} {
		for _, tc := range []struct {
			name      string
			targetCap int64
			want      int64
		}{
			{name: "no target cap", want: 8192},
			{name: "target cap", targetCap: 16000, want: 16000},
		} {
			t.Run(string(protocol)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				const providerModelID = "judge-deployment"
				selection := modelrouter.Selection{Provider: "example-provider", LogicalModel: model}
				var got []int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					var payload struct {
						MaxTokens        int64 `json:"max_tokens"`
						GenerationConfig struct {
							MaxOutputTokens int64 `json:"maxOutputTokens"`
						} `json:"generationConfig"`
					}
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Errorf("decoding request body: %v", err)
					}
					if protocol == modelrouter.ProtocolGoogleGenAI {
						got = append(got, payload.GenerationConfig.MaxOutputTokens)
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"finishReason":"STOP"}]}`, judgementJSON(StandaloneMode))
						return
					}
					got = append(got, payload.MaxTokens)
					writeAnthropicJudgement(w, providerModelID, StandaloneMode)
				}))
				t.Cleanup(server.Close)

				registrations := targetTestRegistrations(t, selection.Provider, server)
				runtime, err := metaagent.NewRuntime(
					[]modelrouter.Route{judgeTestRoute(selection, protocol, providerModelID)},
					metaagent.NewAdapterBackend("", selection.Provider, func(string) (metaagent.AdapterRegistrations, error) {
						return registrations, nil
					}),
				)
				if err != nil {
					t.Fatal(err)
				}
				judgeInstance, err := NewWithTarget(t.Context(), runtime.Target(metaagent.TargetConfig{
					Provider: selection.Provider, Model: selection.LogicalModel, MaxTokens: tc.targetCap,
				}))
				if err != nil {
					t.Fatalf("NewWithTarget: %v", err)
				}
				if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
					t.Fatalf("Judge: %v", err)
				}
				if len(got) != 1 || got[0] != tc.want {
					t.Errorf("output caps sent: got = %v, want = [%d]", got, tc.want)
				}
			})
		}
	}
}

func TestNewWithTargetRejectsNegativeMaxTokens(t *testing.T) {
	t.Parallel()
	selection := modelrouter.Selection{Provider: "example-provider", LogicalModel: "claude-sonnet-4-6"}
	runtime, err := metaagent.NewRuntime(
		[]modelrouter.Route{judgeTestRoute(selection, modelrouter.ProtocolAnthropicMessages, "judge-deployment")},
		metaagent.NewAdapterBackend("", selection.Provider, func(string) (metaagent.AdapterRegistrations, error) {
			t.Error("adapter factory invoked for an invalid target")
			return metaagent.AdapterRegistrations{}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewWithTarget(t.Context(), runtime.Target(metaagent.TargetConfig{Provider: selection.Provider, Model: selection.LogicalModel, MaxTokens: -1}))
	if err == nil {
		t.Fatal("NewWithTarget succeeded with a negative cap")
	}
}

// targetTestRegistrations binds both judge protocols to server without
// credentials.
func targetTestRegistrations(t *testing.T, provider modelrouter.Provider, server *httptest.Server) metaagent.AdapterRegistrations {
	t.Helper()
	messages := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey("explicit-route-token"),
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0),
	).Messages
	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     "routing-test-project",
		Location:    "us-central1",
		HTTPClient:  server.Client(),
		HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	return metaagent.AdapterRegistrations{
		AnthropicMessages: []metaagent.AnthropicMessagesRegistration{{
			Provider: provider,
			Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
				return metaagent.NewAnthropicMessagesBinding(plan, messages, nil)
			},
		}},
		GoogleGenAI: []metaagent.GoogleGenAIRegistration{{
			Provider: provider,
			Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.GoogleGenAIBinding, error) {
				return metaagent.NewGoogleGenAIBinding(plan, client, nil)
			},
		}},
	}
}
