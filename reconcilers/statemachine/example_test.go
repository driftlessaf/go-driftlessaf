/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statemachine_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/statemachine"
)

func ExampleStatus() {
	fmt.Println(statemachine.StatusActive)
	fmt.Println(statemachine.StatusComplete)
	fmt.Println(statemachine.StatusFailed)
	// Output:
	// active
	// complete
	// failed
}

func ExampleFailureMode() {
	fmt.Println(statemachine.FailureModeMaxTurns)
	fmt.Println(statemachine.FailureModePRClosed)
	fmt.Println(statemachine.FailureModeNoDiff)
	fmt.Println(statemachine.FailureModeNoProgress)
	// Output:
	// max_turns
	// pr_closed
	// no_diff
	// no_progress
}

func ExampleWithActor() {
	ctx := context.Background()
	ctx = statemachine.WithActor(ctx, "my-bot")
	actor, ok := statemachine.ActorFromContext(ctx)
	fmt.Println(actor, ok)
	// Output:
	// my-bot true
}

func ExampleWithTrigger() {
	ctx := context.Background()
	ctx = statemachine.WithTrigger(ctx, statemachine.TriggerPRMerge)
	trigger, ok := statemachine.TriggerFromContext(ctx)
	fmt.Println(trigger, ok)
	// Output:
	// pr_merge true
}

func ExampleNewEmitter() {
	// A nil client disables emission; NewEmitter returns nil safely.
	e := statemachine.NewEmitter("my-bot", nil)
	fmt.Println(e)
	// Output:
	// <nil>
}
