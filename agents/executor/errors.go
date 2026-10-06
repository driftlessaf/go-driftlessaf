/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package executor

import "errors"

// ErrMaxTurns reports that an agent run stopped because it reached its
// conversation-turn budget before the agent submitted a result. Every executor
// wraps it, so a caller can recognize the condition with errors.Is regardless
// of which provider ran the agent and decide what to do with the run's partial
// work.
var ErrMaxTurns = errors.New("agent exceeded maximum conversation turns")

// ErrInputTooLarge reports that a model request exceeded the input budget of
// the model's serving context window and was rejected before the provider was
// called. Resending the same conversation fails the same way, so callers
// should treat it as non-retriable. Executors that enforce an input budget
// wrap it, so a caller can recognize the condition with errors.Is without
// importing a provider-specific error type.
var ErrInputTooLarge = errors.New("agent request exceeds model input budget")
