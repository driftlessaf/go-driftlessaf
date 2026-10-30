/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// TestCacheTTLBackends pins that CacheTTL reaches claudeexecutor.WithCacheTTL
// on the Claude backend and is a construction error on the others, never
// silently dropped. The Claude rows observe the option through the
// executor's own check: a TTL other than 5m or 1h fails only when the option
// is applied.
func TestCacheTTLBackends(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		ttl     time.Duration
		wantErr string
	}{{
		name:  "claude one hour",
		model: "claude-opus-5-5",
		ttl:   time.Hour,
	}, {
		name:  "claude five minutes",
		model: "claude-opus-5-5",
		ttl:   5 * time.Minute,
	}, {
		name:    "claude unsupported ttl",
		model:   "claude-opus-5-5",
		ttl:     10 * time.Minute,
		wantErr: "cache TTL must be 5m or 1h",
	}, {
		name:  "claude unset",
		model: "claude-opus-5-5",
	}, {
		name:    "gemini rejects",
		model:   "gemini-2.5-flash",
		ttl:     time.Hour,
		wantErr: "CacheTTL is not supported on the Gemini backend",
	}, {
		name:    "openai-compatible rejects",
		model:   "google/gemini-2.5-pro",
		ttl:     time.Hour,
		wantErr: "CacheTTL is not supported on the OpenAI-compatible backend",
	}, {
		name:  "gemini unset",
		model: "gemini-2.5-flash",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
			cfg := finalizeConfig(t, 0)
			cfg.CacheTTL = tc.ttl
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

// TestRoutedCacheTTLProtocolGate pins the routed twin: only the Anthropic
// Messages protocol accepts a CacheTTL, and every protocol accepts it unset.
func TestRoutedCacheTTLProtocolGate(t *testing.T) {
	for _, tc := range []struct {
		protocol modelrouter.Protocol
		ttl      time.Duration
		wantErr  bool
	}{
		{protocol: modelrouter.ProtocolAnthropicMessages, ttl: time.Hour},
		{protocol: modelrouter.ProtocolGoogleGenAI, ttl: time.Hour, wantErr: true},
		{protocol: modelrouter.ProtocolOpenAIChatCompletions, ttl: time.Hour, wantErr: true},
		{protocol: modelrouter.ProtocolOpenAIResponses, ttl: time.Hour, wantErr: true},
		{protocol: modelrouter.ProtocolSystemOne, ttl: time.Hour, wantErr: true},
		{protocol: modelrouter.ProtocolGoogleGenAI},
		{protocol: modelrouter.ProtocolOpenAIResponses},
	} {
		t.Run(fmt.Sprintf("%s/ttl=%v", tc.protocol, tc.ttl), func(t *testing.T) {
			cfg := finalizeConfig(t, 0)
			cfg.CacheTTL = tc.ttl
			err := validateRoutedConfigForProtocol(tc.protocol, "", cfg)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("validateRoutedConfigForProtocol(%s, ttl=%v): got err = %v, want error = %v", tc.protocol, tc.ttl, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "CacheTTL is supported only on the Anthropic Messages protocol") {
				t.Errorf("validateRoutedConfigForProtocol(%s): got err = %v, want the protocol error", tc.protocol, err)
			}
		})
	}
}
