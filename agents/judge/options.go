/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import "chainguard.dev/driftlessaf/agents/effort"

// Option configures a judge. NewVertex and NewVertexWithLabels accept it
// among their options, and NewRouted accepts it directly. An Option applies to
// whichever backend the model selects.
type Option func(*settings)

type settings struct {
	effort effort.Level
}

// WithEffort sets the reasoning effort of every judgement. Each backend maps
// the level onto its own control through its executor's WithEffort option:
// output_config.effort on Claude, a thinking level on Gemini 3 and later, and
// a thinking budget on earlier Gemini models. Gemini counts that budget
// against the output cap, so on those models the judge raises its cap by the
// budget; an executor WithMaxOutputTokens passed to NewVertex still sets the
// cap. A routed judge also requires its route to declare the level, and fails
// construction otherwise. An empty level sends no effort, as if the option
// were absent, so the model keeps its default.
func WithEffort(level effort.Level) Option {
	return func(s *settings) { s.effort = level }
}
