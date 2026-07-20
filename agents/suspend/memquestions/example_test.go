/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package memquestions_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/suspend/memquestions"
)

// ExampleNew demonstrates the nonce-bound question/answer round-trip: an
// answer only surfaces while it is bound to the currently pending question,
// and Consume with the matching nonce retires it so it cannot replay.
func ExampleNew() {
	ctx := context.Background()
	store := memquestions.New()

	if err := store.Ask(ctx, "org/repo#42", suspend.Question{
		ID:     "nonce-1",
		Key:    "org/repo#42",
		Prompt: "Should I force-push?",
	}); err != nil {
		fmt.Println("error:", err)
		return
	}

	// The question is pending but unanswered: nothing surfaces yet.
	pending, ok, err := store.Pending(ctx, "org/repo#42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("pending:", ok, pending.Prompt)
	_, ok, err = store.Answer(ctx, "org/repo#42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("answered:", ok)

	// A human replies; the store binds the answer to the pending nonce.
	q, _ := store.Provide(ctx, "org/repo#42", "Yes, go ahead.")
	ans, ok, err := store.Answer(ctx, "org/repo#42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("answered:", ok, ans.Text)
	fmt.Println("bound to pending question:", ans.QuestionID == q.ID)

	// Consume retires the question, so the same answer never surfaces twice.
	if err := store.Consume(ctx, "org/repo#42", q.ID); err != nil {
		fmt.Println("error:", err)
		return
	}
	_, ok, err = store.Answer(ctx, "org/repo#42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("after consume:", ok)
	// Output:
	// pending: true Should I force-push?
	// answered: false
	// answered: true Yes, go ahead.
	// bound to pending question: true
	// after consume: false
}
