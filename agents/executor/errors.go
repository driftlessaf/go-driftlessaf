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
