/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package executor_test

import (
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor"
)

// An executor wraps ErrMaxTurns with the budget it enforced; callers match the
// condition with errors.Is rather than by message.
func ExampleErrMaxTurns() {
	err := fmt.Errorf("%w (%d)", executor.ErrMaxTurns, 200)
	fmt.Println(errors.Is(err, executor.ErrMaxTurns))
	fmt.Println(err)
	// Output:
	// true
	// agent exceeded maximum conversation turns (200)
}
