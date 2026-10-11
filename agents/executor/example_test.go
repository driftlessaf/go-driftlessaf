/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package executor_test

import (
	"encoding/json"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/checkpoint"
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

// A MaxTurnsError marshals without its Continuation: the transcript in its
// provider state never reaches a status record that serializes the error.
func ExampleMaxTurnsError_json() {
	err := &executor.MaxTurnsError{Turns: 16, Continuation: &checkpoint.Envelope{
		ProviderState: json.RawMessage(`{"system":"the whole transcript"}`),
	}}
	b, merr := json.Marshal(err)
	if merr != nil {
		fmt.Println(merr)
		return
	}
	fmt.Println(string(b))
	// Output:
	// {"Turns":16}
}

// A MaxTurnsError still matches ErrMaxTurns; a caller that can continue the
// conversation reads its Continuation, and one that cannot sees nil.
func ExampleMaxTurnsError() {
	var err error = &executor.MaxTurnsError{Turns: 16}
	fmt.Println(errors.Is(err, executor.ErrMaxTurns))
	fmt.Println(err)
	if exhausted, ok := errors.AsType[*executor.MaxTurnsError](err); ok {
		fmt.Println(exhausted.Continuation != nil)
	}
	// Output:
	// true
	// agent exceeded maximum conversation turns (16)
	// false
}
