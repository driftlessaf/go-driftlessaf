/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"strings"
	"testing"

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
			err := validateRoutedConfigForProtocol(tc.protocol, cfg)
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
