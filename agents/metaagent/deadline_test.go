/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// TestDeadlineOptionsBackends pins the fail-closed contract for
// FinalizeBeforeDeadline and MinRequestBudget on both construction paths:
// wired on Claude, refused everywhere else and when negative, so a caller
// relying on either never gets a loop that ignores the deadline.
func TestDeadlineOptionsBackends(t *testing.T) {
	type cfgT = Config[*testResponse, testCallbacks]
	for _, field := range []struct {
		name string
		set  func(*cfgT, time.Duration)
		// thinkingConflict is whether the field forces the submit tool, which
		// an explicit thinking budget forbids.
		thinkingConflict bool
	}{
		{name: "FinalizeBeforeDeadline", set: func(c *cfgT, d time.Duration) { c.FinalizeBeforeDeadline = d }, thinkingConflict: true},
		{name: "MinRequestBudget", set: func(c *cfgT, d time.Duration) { c.MinRequestBudget = d }},
	} {
		for _, tc := range []struct {
			name    string
			model   string
			value   time.Duration
			wantErr string
		}{
			{name: "claude accepts", model: "claude-test-model", value: time.Minute},
			{name: "gemini rejects", model: "gemini-2.5-flash", value: time.Minute, wantErr: "not supported on the Gemini backend"},
			{name: "openai-compatible rejects", model: "google/gemini-2.5-pro", value: time.Minute, wantErr: "not supported on the OpenAI-compatible backend"},
			{name: "claude rejects negative", model: "claude-test-model", value: -time.Second, wantErr: "must be non-negative"},
		} {
			t.Run(field.name+"/"+tc.name, func(t *testing.T) {
				t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
				cfg := finalizeConfig(t, 0)
				field.set(&cfg, tc.value)
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

		t.Run(field.name+"/claude with thinking budget", func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
			cfg := finalizeConfig(t, 0)
			cfg.ThinkingBudget = 2048
			field.set(&cfg, time.Minute)
			_, err := New[*testRequest](t.Context(), "test-project", "us-central1", "claude-test-model", cfg)
			if gotErr := err != nil && strings.Contains(err.Error(), "incompatible with ThinkingBudget"); gotErr != field.thinkingConflict {
				t.Errorf("New() error = %v, want a ThinkingBudget conflict = %v", err, field.thinkingConflict)
			}
		})

		cfg := finalizeConfig(t, 0)
		field.set(&cfg, time.Minute)
		for _, tc := range []struct {
			protocol modelrouter.Protocol
			wantErr  bool
		}{
			{protocol: modelrouter.ProtocolAnthropicMessages},
			{protocol: modelrouter.ProtocolGoogleGenAI, wantErr: true},
			{protocol: modelrouter.ProtocolOpenAIChatCompletions, wantErr: true},
		} {
			t.Run(field.name+"/routed "+string(tc.protocol), func(t *testing.T) {
				err := validateRoutedConfigForProtocol(tc.protocol, "", cfg)
				if gotErr := err != nil; gotErr != tc.wantErr {
					t.Errorf("validateRoutedConfigForProtocol(%s): got err = %v, want error = %v", tc.protocol, err, tc.wantErr)
				}
			})
		}
		field.set(&cfg, -time.Second)
		if err := validateRoutedConfig(cfg); err == nil || !strings.Contains(err.Error(), "must not be negative") {
			t.Errorf("validateRoutedConfig(negative %s): got err = %v, want a must-not-be-negative error", field.name, err)
		}
	}
}
