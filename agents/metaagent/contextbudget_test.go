/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// TestContextBudgetBackends pins that ContextBudget reaches its
// claudeexecutor option on the Claude backend and is a construction error on
// the others. The Claude rows observe the option through the executor's own
// validation: a negative budget fails only when the option is applied.
func TestContextBudgetBackends(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		budget  claudeexecutor.ContextBudget
		wantErr string
	}{{
		name:   "claude accepts",
		model:  "claude-haiku-5-5",
		budget: claudeexecutor.ContextBudget{Tokens: 85_000, KeepRecentTurns: 4},
	}, {
		name:  "claude off",
		model: "claude-haiku-5-5",
	}, {
		name:    "claude applies the option",
		model:   "claude-haiku-5-5",
		budget:  claudeexecutor.ContextBudget{Tokens: -1},
		wantErr: "context budget tokens must not be negative",
	}, {
		name:    "gemini rejects",
		model:   "gemini-2.5-flash",
		budget:  claudeexecutor.ContextBudget{Tokens: 85_000},
		wantErr: "ContextBudget is not supported on the Gemini backend",
	}, {
		name:  "gemini off",
		model: "gemini-2.5-flash",
	}, {
		name:    "openai-compatible rejects",
		model:   "google/gemini-2.5-pro",
		budget:  claudeexecutor.ContextBudget{Tokens: 85_000},
		wantErr: "ContextBudget is not supported on the OpenAI-compatible backend",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
			cfg := finalizeConfig(t, 0)
			cfg.ContextBudget = tc.budget
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

// TestRoutedContextBudgetProtocolGate pins the routed twin: only the
// Anthropic Messages protocol accepts ContextBudget, and a negative budget
// is refused before any protocol is considered.
func TestRoutedContextBudgetProtocolGate(t *testing.T) {
	for _, tc := range []struct {
		protocol modelrouter.Protocol
		tokens   int64
		wantErr  string
	}{
		{protocol: modelrouter.ProtocolAnthropicMessages, tokens: 85_000},
		{protocol: modelrouter.ProtocolGoogleGenAI, tokens: 85_000, wantErr: "ContextBudget is supported only on the Anthropic Messages protocol"},
		{protocol: modelrouter.ProtocolOpenAIChatCompletions, tokens: 85_000, wantErr: "ContextBudget is supported only on the Anthropic Messages protocol"},
		{protocol: modelrouter.ProtocolOpenAIResponses, tokens: 85_000, wantErr: "ContextBudget is supported only on the Anthropic Messages protocol"},
		{protocol: modelrouter.ProtocolSystemOne, tokens: 85_000, wantErr: "ContextBudget is supported only on the Anthropic Messages protocol"},
		{protocol: modelrouter.ProtocolGoogleGenAI},
		{protocol: modelrouter.ProtocolOpenAIResponses},
	} {
		cfg := finalizeConfig(t, 0)
		cfg.ContextBudget.Tokens = tc.tokens
		err := validateRoutedConfigForProtocol(tc.protocol, "", cfg)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("validateRoutedConfigForProtocol(%s, tokens=%d): got err = %v, want nil", tc.protocol, tc.tokens, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("validateRoutedConfigForProtocol(%s, tokens=%d): got err = %v, want containing %q", tc.protocol, tc.tokens, err, tc.wantErr)
		}
	}

	cfg := finalizeConfig(t, 0)
	cfg.ContextBudget.Tokens = -1
	if err := validateRoutedConfig(cfg); err == nil || !strings.Contains(err.Error(), "context budget tokens must not be negative") {
		t.Errorf("validateRoutedConfig(tokens=-1): got err = %v, want the negative-budget error", err)
	}
}
