/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package effort

import "fmt"

// Level is the provider-neutral reasoning-effort scale shared by every
// executor backend. It controls how deeply a model thinks and its overall
// token spend. The scale follows the Anthropic effort levels — the superset
// across providers — and each backend maps it onto its native control:
//
//   - Claude: output_config.effort, an identity mapping
//     (claudeexecutor.WithEffort).
//   - Gemini: thinkingLevel on Gemini 3.x models, thinkingBudget tiers on
//     earlier models (googleexecutor.WithEffort).
//   - OpenAI-compatible: reasoning_effort, where XHigh and Max clamp to
//     "high" (openaiexecutor.WithEffort).
//
// The zero value means "not configured": backends keep their model default,
// and Validate rejects it — express "no effort" by not passing the option.
type Level string

const (
	// Low suits short, scoped tasks and latency-sensitive work that is not
	// intelligence-sensitive.
	Low Level = "low"
	// Medium trades some depth for cost; a step down from each backend's
	// default.
	Medium Level = "medium"
	// High balances depth and token spend; it is the Claude model default.
	High Level = "high"
	// XHigh is the recommended setting for hard coding/agentic work on
	// Claude. Backends without an equivalent clamp it to their maximum.
	XHigh Level = "xhigh"
	// Max spends whatever it takes; reserve it for correctness-critical work.
	// Backends without an equivalent clamp it to their maximum.
	Max Level = "max"
)

// Validate returns an error unless l is one of the defined levels. The empty
// string is invalid: leaving effort unconfigured is expressed by not passing
// the executor option, not by passing an empty Level.
func (l Level) Validate() error {
	switch l {
	case Low, Medium, High, XHigh, Max:
		return nil
	default:
		return fmt.Errorf("invalid effort level %q (want low|medium|high|xhigh|max)", string(l))
	}
}
