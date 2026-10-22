/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

func finalizeConfig(t *testing.T, n int) Config[*testResponse, testCallbacks] {
	t.Helper()
	userPrompt, err := promptbuilder.NewPrompt("payload")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	return Config[*testResponse, testCallbacks]{
		UserPrompt: userPrompt,
		Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
			toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
				toolcall.NewEmptyToolsProvider[*testResponse]())),
		MaxToolCallsBeforeFinalize: n,
	}
}

// TestMaxToolCallsBeforeFinalizeBackends pins the fail-closed contract: the
// soft cap is wired on Claude and a construction error elsewhere, never
// silently dropped — a caller relying on it to bound a run would otherwise
// get an uncapped loop.
func TestMaxToolCallsBeforeFinalizeBackends(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		mutate  func(*Config[*testResponse, testCallbacks])
		wantErr string
	}{
		{name: "claude accepts", model: "claude-test-model"},
		{name: "gemini rejects", model: "gemini-2.5-flash", wantErr: "not supported on the Gemini backend"},
		{name: "openai-compatible rejects", model: "google/gemini-2.5-pro", wantErr: "not supported on the OpenAI-compatible backend"},
		{
			name:    "claude rejects with thinking budget",
			model:   "claude-test-model",
			mutate:  func(c *Config[*testResponse, testCallbacks]) { c.ThinkingBudget = 2048 },
			wantErr: "incompatible with ThinkingBudget",
		},
		{
			name:    "claude rejects negative",
			model:   "claude-test-model",
			mutate:  func(c *Config[*testResponse, testCallbacks]) { c.MaxToolCallsBeforeFinalize = -1 },
			wantErr: "must be non-negative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
			cfg := finalizeConfig(t, 12)
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			_, err := New[*testRequest](t.Context(), "test-project", "us-central1", tc.model, cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("New() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("New() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestRoutedFinalizeProtocolGate pins the routed path's twin of the backend
// rejection: only the Anthropic Messages protocol accepts the soft cap, and a
// negative value is refused before any protocol is considered.
func TestRoutedFinalizeProtocolGate(t *testing.T) {
	cfg := finalizeConfig(t, 12)
	for _, tc := range []struct {
		protocol modelrouter.Protocol
		wantErr  bool
	}{
		{protocol: modelrouter.ProtocolAnthropicMessages},
		{protocol: modelrouter.ProtocolGoogleGenAI, wantErr: true},
		{protocol: modelrouter.ProtocolOpenAIChatCompletions, wantErr: true},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			err := validateRoutedConfigForProtocol(tc.protocol, "", cfg)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("validateRoutedConfigForProtocol(%s): got err = %v, want error = %v", tc.protocol, err, tc.wantErr)
			}
		})
	}

	neg := finalizeConfig(t, -1)
	if err := validateRoutedConfig(neg); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("validateRoutedConfig(negative): got err = %v, want a must-not-be-negative error", err)
	}
}

// TestPromptTokensBeforeFinalizeBackends pins the same fail-closed contract
// for the prompt budget, on both construction paths: wired on Claude, refused
// everywhere else and when negative.
func TestPromptTokensBeforeFinalizeBackends(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		mutate  func(*Config[*testResponse, testCallbacks])
		wantErr string
	}{
		{name: "claude accepts", model: "claude-test-model"},
		{name: "gemini rejects", model: "gemini-2.5-flash", wantErr: "not supported on the Gemini backend"},
		{name: "openai-compatible rejects", model: "google/gemini-2.5-pro", wantErr: "not supported on the OpenAI-compatible backend"},
		{
			name:    "claude rejects with thinking budget",
			model:   "claude-test-model",
			mutate:  func(c *Config[*testResponse, testCallbacks]) { c.ThinkingBudget = 2048 },
			wantErr: "incompatible with ThinkingBudget",
		},
		{
			name:    "claude rejects negative",
			model:   "claude-test-model",
			mutate:  func(c *Config[*testResponse, testCallbacks]) { c.PromptTokensBeforeFinalize = -1 },
			wantErr: "must be non-negative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
			cfg := finalizeConfig(t, 0)
			cfg.PromptTokensBeforeFinalize = 800_000
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			_, err := New[*testRequest](t.Context(), "test-project", "us-central1", tc.model, cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("New() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("New() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	cfg := finalizeConfig(t, 0)
	cfg.PromptTokensBeforeFinalize = 800_000
	for _, tc := range []struct {
		protocol modelrouter.Protocol
		wantErr  bool
	}{
		{protocol: modelrouter.ProtocolAnthropicMessages},
		{protocol: modelrouter.ProtocolGoogleGenAI, wantErr: true},
		{protocol: modelrouter.ProtocolOpenAIChatCompletions, wantErr: true},
	} {
		t.Run("routed "+string(tc.protocol), func(t *testing.T) {
			err := validateRoutedConfigForProtocol(tc.protocol, "", cfg)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("validateRoutedConfigForProtocol(%s): got err = %v, want error = %v", tc.protocol, err, tc.wantErr)
			}
		})
	}
	cfg.PromptTokensBeforeFinalize = -1
	if err := validateRoutedConfig(cfg); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("validateRoutedConfig(negative): got err = %v, want a must-not-be-negative error", err)
	}
}

// TestPromptTokensBeforeFinalizeReachesTheExecutor drives a routed Claude
// agent whose first turn is sent a 7-token prompt: with a 5-token budget the
// second request must carry the instruction, which only happens if the Config
// value reaches the executor.
func TestPromptTokensBeforeFinalizeReachesTheExecutor(t *testing.T) {
	var requests atomic.Int32
	var second string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		name, args := "lookup", `{"reasoning":"read fixture"}`
		if n == 2 {
			second = string(body)
			name, args = "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`
		}
		writeToolUseTurn(t, w, n, name, args)
	}))
	t.Cleanup(srv.Close)

	selection := modelrouter.Selection{Provider: "scripted", LogicalModel: "claude-sonnet-4-6"}
	adapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{
		Provider: selection.Provider,
		Adapter: func(_ context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("scripted"), option.WithMaxRetries(0))
			return NewAnthropicMessagesBinding(plan, client.Messages, nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, routedTestRoute(selection, modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6")),
		AdapterRegistries{AnthropicMessages: adapters})

	cfg := routedTestConfig(t)
	cfg.Tools = new(bedrockChatTools)
	cfg.PromptTokensBeforeFinalize = 5
	agent, err := NewRouted[*testRequest](t.Context(), router, selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := agenttrace.WithTracer[*basetenTestResponse](t.Context(), new(basetenRecordingTracer))
	if _, err := agent.Execute(ctx, &testRequest{}, toolcall.EmptyTools{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(second, "close to the model's context limit") {
		t.Error("second request lacks the prompt-budget instruction: the Config value did not reach the executor")
	}
}
