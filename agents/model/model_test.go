/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package model_test

import (
	"fmt"
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/model"
	"github.com/google/go-cmp/cmp"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	fullScale := []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}
	preXHigh := []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max}

	claudeNoEffort := model.Info{
		Backend:                model.BackendClaude,
		SamplingParams:         true,
		ExtendedThinkingBudget: true,
	}
	claudePreXHigh := model.Info{
		Backend:                model.BackendClaude,
		Efforts:                preXHigh,
		SamplingParams:         true,
		ExtendedThinkingBudget: true,
	}
	claudeAdaptive := model.Info{
		Backend: model.BackendClaude,
		Efforts: fullScale,
	}
	// claudeAutoToolChoice is the adaptive surface for models that also reject
	// a forced tool_choice: Fable 5.1, Opus 5.5, and Sonnet 5.5.
	claudeAutoToolChoice := claudeAdaptive
	claudeAutoToolChoice.AutomaticToolChoiceOnly = true
	claudeOpus55 := claudeAutoToolChoice
	claudeOpus55.ContextWindow = 1_000_000
	claudeOpus48 := claudeAdaptive
	claudeOpus48.ContextWindow = 1_000_000
	geminiBudget := model.Info{
		Backend:         model.BackendGemini,
		Efforts:         fullScale,
		SamplingParams:  true,
		ThinkingControl: model.ThinkingControlBudget,
	}
	geminiLevel := model.Info{
		Backend:         model.BackendGemini,
		Efforts:         fullScale,
		SamplingParams:  true,
		ThinkingControl: model.ThinkingControlLevel,
	}
	openAICompat := model.Info{
		Backend:        model.BackendOpenAICompat,
		Efforts:        fullScale,
		SamplingParams: true,
	}
	openAICompatExplicitRoute := openAICompat
	openAICompatExplicitRoute.ExplicitRouteOnly = true
	systemOne := model.Info{Backend: model.BackendSystemOne}

	tests := []struct {
		id   string
		want model.Info
	}{
		// Backend routing shapes, including unknown ids.
		{"gemini-2.5-flash", geminiBudget},
		{"claude-fable-5", claudeAdaptive},
		{"claude-mythos-5-1", claudeAdaptive},
		{"claude-fable-5-1", claudeAutoToolChoice},
		// Haiku 5.5 takes the adaptive surface but accepts a forced tool_choice.
		{"claude-haiku-5-5", claudeAdaptive},
		{"claude-haiku-5-5@default", claudeAdaptive},
		{"anthropic.claude-haiku-5-5", claudeAdaptive},
		{"claude-fable-5-1@default", claudeAutoToolChoice},
		{"anthropic.claude-fable-5-1", claudeAutoToolChoice},
		{"claude-fable-5-10", claudeAdaptive},
		{"claude-opus-5-5", claudeOpus55},
		{"claude-opus-5-5@default", claudeOpus55},
		{"claude-opus-5", claudeAdaptive},
		{"claude-sonnet-5-5", claudeAutoToolChoice},
		{"claude-sonnet-5-5@default", claudeAutoToolChoice},
		{"anthropic.claude-sonnet-5-5", claudeAutoToolChoice},
		{"anthropic.claude-sonnet-5", claudeAdaptive},
		{"anthropic.claude-sonnet-4-6", claudePreXHigh},
		{"meta/llama-3.3-70b-instruct-maas", openAICompat},
		{"mistralai/mistral-large", openAICompat},
		{"google/gemini-3.5-flash", openAICompat},
		{"gpt-4o", openAICompat},
		{"gpt-5", openAICompat},
		{"gpt-5.6-terra", openAICompat},
		{"GPT-5.6-TERRA", openAICompat},
		// xAI ids need an explicit route; the publisher match is
		// case-insensitive, like the backend routing shapes.
		{"xai/grok-4.7", openAICompatExplicitRoute},
		{"XAI/Grok-4.7", openAICompatExplicitRoute},
		// System One ids carry no parameter surface; aliases and versioned ids
		// resolve alike and the prefix match is case-insensitive.
		{"jev-latest", systemOne},
		{"jev-preview", systemOne},
		{"jev-1.13.0", systemOne},
		{"JEV-LATEST", systemOne},
		{"hopper", systemOne},
		{"jev", model.Info{}},
		{"us.openai.gpt-5.6-terra", model.Info{}},
		{"text-bison", model.Info{}},
		{"", model.Info{}},
		// Backend routing is case-insensitive, matching the metaagent
		// dispatch. Capability tables match case-sensitively against the id
		// as given, mirroring the executors: an upper-cased Claude id misses
		// the exception prefixes and keeps the default surface, and an
		// upper-cased Gemini id has no parseable major version so it falls
		// back to the budget control.
		{"CLAUDE-FABLE-5", model.Info{
			Backend:                model.BackendClaude,
			Efforts:                fullScale,
			SamplingParams:         true,
			ExtendedThinkingBudget: true,
		}},
		{"Gemini-3-Pro", geminiBudget},
		// Claude ids that predate the effort parameter.
		{"claude-2", claudeNoEffort},
		{"claude-2.1", claudeNoEffort},
		{"claude-3-5-sonnet-20241022", claudeNoEffort},
		{"claude-instant-1.2", claudeNoEffort},
		{"claude-haiku-4-5", claudeNoEffort},
		{"claude-haiku-4-5@20251001", claudeNoEffort},
		{"anthropic.claude-haiku-4-5", claudeNoEffort},
		{"claude-sonnet-4@default", claudeNoEffort},
		{"claude-sonnet-4-0", claudeNoEffort},
		{"claude-sonnet-4-5", claudeNoEffort},
		{"claude-sonnet-4-5@20250929", claudeNoEffort},
		{"claude-opus-4@20250514", claudeNoEffort},
		{"claude-opus-4-0", claudeNoEffort},
		{"claude-opus-4-1", claudeNoEffort},
		{"claude-opus-4-1@default", claudeNoEffort},
		// Claude ids that take effort but predate the xhigh level.
		{"claude-sonnet-4-6", claudePreXHigh},
		{"claude-sonnet-4-6@default", claudePreXHigh},
		{"claude-opus-4-5", claudePreXHigh},
		{"claude-opus-4-5@20251101", claudePreXHigh},
		{"claude-opus-4-6@default", claudePreXHigh},
		// Claude ids with the Opus 4.7 surface: full effort scale, sampling
		// params and extended-thinking budget removed.
		{"claude-opus-4-7", claudeAdaptive},
		{"claude-opus-4-8@default", claudeOpus48},
		{"claude-fable-5@default", claudeAdaptive},
		{"claude-mythos-5-1@default", claudeAdaptive},
		{"claude-sonnet-5@20260301", claudeAdaptive},
		// Future Claude ids keep the newest surface by default; they miss
		// the sampling-removed table too, so those parameters stay accepted
		// until the id is verified to reject them.
		{"claude-sonnet-6", model.Info{
			Backend:                model.BackendClaude,
			Efforts:                fullScale,
			SamplingParams:         true,
			ExtendedThinkingBudget: true,
		}},
		// Opus 5 is a known 5-series model: it shares the adaptive-thinking
		// surface (sampling params and the thinking-budget param removed),
		// like Opus 4.8, Sonnet 5, and Fable 5.
		{"claude-opus-5", claudeAdaptive},
		// Gemini thinking-knob generations.
		{"gemini-1.5-pro", geminiBudget},
		{"gemini-2.5-pro", geminiBudget},
		{"gemini-3-pro-preview", geminiLevel},
		{"gemini-3.5-flash", geminiLevel},
		{"gemini-4-ultra", geminiLevel},
		// Unparseable Gemini versions fall back to the budget control.
		{"gemini-ultra", geminiBudget},
		{"gemini-exp-1206", geminiBudget},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, model.Resolve(tt.id)); diff != "" {
				t.Errorf("Resolve(%q) mismatch (-want, +got):\n%s", tt.id, diff)
			}
		})
	}
}

func TestInfoSupportsEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id    string
		level effort.Level
		want  bool
	}{
		{"claude-fable-5", effort.XHigh, true},
		{"claude-fable-5", effort.Max, true},
		{"claude-opus-4-6", effort.XHigh, false},
		{"claude-opus-4-6", effort.Max, true},
		{"claude-sonnet-4-5", effort.High, false},
		{"claude-sonnet-4-5", effort.Low, false},
		{"gemini-2.5-flash", effort.Max, true},
		{"gemini-3-pro-preview", effort.XHigh, true},
		{"meta/llama-3.3-70b-instruct-maas", effort.XHigh, true},
		{"gpt-5.6-terra", effort.Low, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s@%s", tt.id, tt.level), func(t *testing.T) {
			t.Parallel()
			if got := model.Resolve(tt.id).SupportsEffort(tt.level); got != tt.want {
				t.Errorf("Resolve(%q).SupportsEffort(%q) = %v, want %v", tt.id, tt.level, got, tt.want)
			}
		})
	}
}

// TestResolveContextWindow pins that only verified base ids report a window:
// a neighbouring id, an older family member or an unknown id reports 0, so an
// input budget cannot run on a guessed limit.
func TestResolveContextWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   string
		want int64
	}{
		{"claude-opus-5-5", 1_000_000},
		{"claude-opus-5-5@default", 1_000_000},
		{"anthropic.claude-opus-5-5", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4-8@20260101", 1_000_000},
		{"claude-opus-4-8-1", 0},
		{"claude-opus-4-7", 0},
		{"claude-opus-5", 0},
		{"claude-sonnet-4-6", 0},
		{"claude-sonnet-5-5@default", 0},
		{"gemini-2.5-pro", 0},
		{"made-up-model-7", 0},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			if got := model.Resolve(tt.id).ContextWindow; got != tt.want {
				t.Errorf("Resolve(%q).ContextWindow: got = %d, want = %d", tt.id, got, tt.want)
			}
		})
	}
}
