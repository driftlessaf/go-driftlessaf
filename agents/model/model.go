/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package model

import (
	"slices"
	"strconv"
	"strings"

	"chainguard.dev/driftlessaf/agents/effort"
)

// Backend identifies the provider surface a model id routes to.
type Backend string

const (
	// BackendClaude covers ids with the "claude-" prefix, served by the
	// Anthropic API.
	BackendClaude Backend = "claude"
	// BackendGemini covers ids with the "gemini-" prefix, served by the
	// Google Generative AI API.
	BackendGemini Backend = "gemini"
	// BackendOpenAICompat covers "publisher/model" ids, served by an
	// OpenAI-compatible endpoint.
	BackendOpenAICompat Backend = "openai-compat"
	// BackendUnknown is the zero value, for ids that match no known routing
	// shape.
	BackendUnknown Backend = ""
)

// ThinkingControl identifies which generation of Gemini thinking knob a
// model takes.
type ThinkingControl string

const (
	// ThinkingControlNone marks backends without a Gemini-style knob.
	ThinkingControlNone ThinkingControl = ""
	// ThinkingControlBudget marks Gemini models before 3.x, which take a
	// thinkingBudget token count.
	ThinkingControlBudget ThinkingControl = "budget"
	// ThinkingControlLevel marks Gemini 3.x and later, which take a discrete
	// thinkingLevel enum.
	ThinkingControlLevel ThinkingControl = "level"
)

// Info describes the provider-side capabilities of a model.
type Info struct {
	// Backend is the provider surface the model id routes to.
	Backend Backend
	// Efforts lists the effort levels the provider accepts natively; empty
	// means the effort parameter is unsupported.
	Efforts []effort.Level
	// SamplingParams reports whether temperature/top_p/top_k are accepted.
	SamplingParams bool
	// ExtendedThinkingBudget reports whether the Claude thinking.budget_tokens
	// parameter is accepted.
	ExtendedThinkingBudget bool
	// ThinkingControl is the Gemini thinking-knob generation the model takes.
	ThinkingControl ThinkingControl
}

// SupportsEffort reports whether the provider accepts the effort level
// natively for this model.
func (i Info) SupportsEffort(l effort.Level) bool {
	return slices.Contains(i.Efforts, l)
}

// fullEfforts is the complete provider-neutral scale. Claude models with the
// Opus 4.7 effort surface accept every level natively; the Gemini and
// OpenAI-compatible executors map the whole scale onto their own controls.
var fullEfforts = []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}

// preXHighEfforts is the scale accepted by effort-capable Claude models that
// predate the "xhigh" level.
var preXHighEfforts = []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max}

// samplingParamsRemovedPrefixes lists Claude id prefixes for which the
// Anthropic API removed the sampling parameters (temperature, top_p, top_k)
// AND the extended-thinking budget parameter (thinking.type="enabled",
// budget_tokens=N) in favor of adaptive thinking. Opus 4.7 introduced this
// surface; Opus 4.8, Sonnet 5, and Fable 5 share it.
// See: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7#sampling-parameters-removed
var samplingParamsRemovedPrefixes = []string{
	"claude-opus-4-7",
	"claude-opus-4-8",
	"claude-fable-5",
	"claude-sonnet-5",
}

// noEffortModelPrefixes lists Claude id prefixes that predate the effort
// parameter (output_config.effort): the API returns a 400 when it is set.
// Unlisted ids — including future ones — are assumed effort-capable, so a
// freshly released model keeps the newest surface by default and this list
// only grows when a model is verified to reject the parameter.
var noEffortModelPrefixes = []string{
	"claude-2",
	"claude-3",
	"claude-instant",
	"claude-haiku",
	"claude-sonnet-4@",
	"claude-sonnet-4-0",
	"claude-sonnet-4-5",
	"claude-opus-4@",
	"claude-opus-4-0",
	"claude-opus-4-1",
}

// preXHighEffortModelPrefixes lists effort-capable Claude id prefixes that
// predate the "xhigh" level and accept low|medium|high|max only. xhigh
// arrived with the Opus 4.7 surface; the API rejects it on these models with
// a 400 naming the accepted values.
var preXHighEffortModelPrefixes = []string{
	"claude-sonnet-4-6",
	"claude-opus-4-5",
	"claude-opus-4-6",
}

// Resolve returns the capability Info for the given model id. The backend is
// determined from the id's routing shape, matching prefixes
// case-insensitively; ids that match no known shape resolve to the zero Info
// (BackendUnknown, no capabilities).
func Resolve(id string) Info {
	lower := strings.ToLower(id)
	switch {
	case strings.HasPrefix(lower, "gemini-"):
		return geminiInfo(id)
	case strings.HasPrefix(lower, "claude-"):
		return claudeInfo(id)
	case strings.Contains(id, "/"):
		// The executor clamps xhigh/max onto the provider's "high"
		// reasoning_effort, so the whole scale is usable.
		return Info{
			Backend:        BackendOpenAICompat,
			Efforts:        slices.Clone(fullEfforts),
			SamplingParams: true,
		}
	default:
		return Info{}
	}
}

// claudeInfo derives the capability surface for a Claude model id. The
// exception-prefix tables match case-sensitively against the id as given,
// mirroring how the Anthropic API matches model names.
func claudeInfo(id string) Info {
	info := Info{Backend: BackendClaude}
	if !hasAnyPrefix(id, samplingParamsRemovedPrefixes) {
		info.SamplingParams = true
		info.ExtendedThinkingBudget = true
	}
	switch {
	case hasAnyPrefix(id, noEffortModelPrefixes):
		// The API rejects the effort parameter; Efforts stays empty.
	case hasAnyPrefix(id, preXHighEffortModelPrefixes):
		info.Efforts = slices.Clone(preXHighEfforts)
	default:
		info.Efforts = slices.Clone(fullEfforts)
	}
	return info
}

// geminiInfo derives the capability surface for a Gemini model id.
func geminiInfo(id string) Info {
	return Info{
		Backend:         BackendGemini,
		Efforts:         slices.Clone(fullEfforts),
		SamplingParams:  true,
		ThinkingControl: geminiThinkingControl(id),
	}
}

// geminiThinkingControl reports which generation of thinking knob the model
// takes: the discrete thinkingLevel enum replaced the thinkingBudget token
// count with Gemini 3. Ids whose major version cannot be determined fall
// back to the budget control.
func geminiThinkingControl(id string) ThinkingControl {
	rest, ok := strings.CutPrefix(id, "gemini-")
	if !ok {
		return ThinkingControlBudget
	}
	version, _, _ := strings.Cut(rest, "-")
	major, _, _ := strings.Cut(version, ".")
	if n, err := strconv.Atoi(major); err == nil && n >= 3 {
		return ThinkingControlLevel
	}
	return ThinkingControlBudget
}

// hasAnyPrefix reports whether id starts with any of the prefixes.
func hasAnyPrefix(id string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}
