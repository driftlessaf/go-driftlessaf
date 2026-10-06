/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// TestEnforceInputBudgetBackends pins that EnforceInputBudget reaches
// claudeexecutor.WithInputBudget on the Claude backend and is a construction
// error on the others, never silently dropped. The Claude rows observe the
// option through the executor's own construction checks: an unknown model
// with no window, a window that max_tokens fills, and a negative window all
// fail only when the option is applied.
func TestEnforceInputBudgetBackends(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		mutate  func(*Config[*testResponse, testCallbacks])
		wantErr string
	}{{
		name:  "claude registry window",
		model: "claude-opus-5-5",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.EnforceInputBudget = true
		},
	}, {
		name:  "claude explicit window on an unknown model",
		model: "claude-test-model",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.EnforceInputBudget = true
			c.InputContextWindow = 200_000
		},
	}, {
		name:  "claude unknown model without a window",
		model: "claude-test-model",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.EnforceInputBudget = true
		},
		wantErr: "no context window is known",
	}, {
		name:  "claude window filled by max_tokens",
		model: "claude-test-model",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.EnforceInputBudget = true
			c.InputContextWindow = 32_000
		},
		wantErr: "leaves no input budget",
	}, {
		name:  "claude negative window",
		model: "claude-test-model",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.EnforceInputBudget = true
			c.InputContextWindow = -1
		},
		wantErr: "must not be negative",
	}, {
		name:  "claude off ignores the window",
		model: "claude-test-model",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.InputContextWindow = -1
		},
	}, {
		name:  "claude off on an unknown model",
		model: "claude-test-model",
	}, {
		name:  "gemini rejects",
		model: "gemini-2.5-flash",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.EnforceInputBudget = true
		},
		wantErr: "EnforceInputBudget is not supported on the Gemini backend",
	}, {
		name:  "openai-compatible rejects",
		model: "google/gemini-2.5-pro",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.EnforceInputBudget = true
		},
		wantErr: "EnforceInputBudget is not supported on the OpenAI-compatible backend",
	}, {
		name:  "gemini off ignores the window",
		model: "gemini-2.5-flash",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.InputContextWindow = 200_000
		},
	}, {
		name:  "openai-compatible off ignores the window",
		model: "google/gemini-2.5-pro",
		mutate: func(c *Config[*testResponse, testCallbacks]) {
			c.InputContextWindow = 200_000
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
			cfg := finalizeConfig(t, 0)
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

// TestRoutedInputBudgetProtocolGate pins the routed twin: only the Anthropic
// Messages protocol accepts EnforceInputBudget, every protocol still accepts
// a config with the budget off, and a negative window is refused before any
// protocol is considered.
func TestRoutedInputBudgetProtocolGate(t *testing.T) {
	for _, tc := range []struct {
		protocol modelrouter.Protocol
		enforce  bool
		wantErr  bool
	}{
		{protocol: modelrouter.ProtocolAnthropicMessages, enforce: true},
		{protocol: modelrouter.ProtocolGoogleGenAI, enforce: true, wantErr: true},
		{protocol: modelrouter.ProtocolOpenAIChatCompletions, enforce: true, wantErr: true},
		{protocol: modelrouter.ProtocolOpenAIResponses, enforce: true, wantErr: true},
		{protocol: modelrouter.ProtocolSystemOne, enforce: true, wantErr: true},
		{protocol: modelrouter.ProtocolAnthropicMessages},
		{protocol: modelrouter.ProtocolGoogleGenAI},
		{protocol: modelrouter.ProtocolOpenAIChatCompletions},
		{protocol: modelrouter.ProtocolOpenAIResponses},
		{protocol: modelrouter.ProtocolSystemOne},
	} {
		cfg := finalizeConfig(t, 0)
		cfg.EnforceInputBudget = tc.enforce
		err := validateRoutedConfigForProtocol(tc.protocol, cfg)
		if gotErr := err != nil; gotErr != tc.wantErr {
			t.Errorf("validateRoutedConfigForProtocol(%s, enforce=%v): got err = %v, want error = %v", tc.protocol, tc.enforce, err, tc.wantErr)
		}
		if tc.wantErr && !strings.Contains(err.Error(), "EnforceInputBudget is supported only on the Anthropic Messages protocol") {
			t.Errorf("validateRoutedConfigForProtocol(%s): got err = %v, want the EnforceInputBudget protocol error", tc.protocol, err)
		}
	}

	for _, tc := range []struct {
		window  int64
		enforce bool
		wantErr bool
	}{
		{window: -1, enforce: true, wantErr: true},
		{window: -1, wantErr: true},
		{window: 0, enforce: true},
		{window: 200_000, enforce: true},
	} {
		cfg := finalizeConfig(t, 0)
		cfg.EnforceInputBudget = tc.enforce
		cfg.InputContextWindow = tc.window
		err := validateRoutedConfig(cfg)
		if gotErr := err != nil; gotErr != tc.wantErr {
			t.Errorf("validateRoutedConfig(window=%d, enforce=%v): got err = %v, want error = %v", tc.window, tc.enforce, err, tc.wantErr)
		}
		if tc.wantErr && !strings.Contains(err.Error(), "input context window must not be negative") {
			t.Errorf("validateRoutedConfig(window=%d): got err = %v, want the negative-window error", tc.window, err)
		}
	}
}
