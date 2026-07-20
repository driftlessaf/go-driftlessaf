/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package suspend_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/memstore"
	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/suspend/memquestions"
	"chainguard.dev/driftlessaf/workqueue"
)

// ExampleCoordinator demonstrates the full halt/wake lifecycle: Suspend parks
// an executor's checkpoint.Suspension and posts the human question, then Wake
// walks the tri-state re-entry — rearm while unanswered, resume exactly once
// after the human answers, fresh thereafter.
func ExampleCoordinator() {
	ctx := context.Background()
	store, questions := memstore.New(), memquestions.New()

	c, err := suspend.New(store, questions, time.Minute)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// An executor suspended on a held-out ask-human call and handed the
	// Suspension up as an ordinary error; the reconciler parks it here.
	err = c.Suspend(ctx, "org/repo#42", &checkpoint.Suspension{
		Envelope: checkpoint.Envelope{
			Version:        checkpoint.EnvelopeVersion,
			Provider:       checkpoint.ProviderAnthropic,
			Model:          "claude-fable-5",
			ConfigDigest:   "sha256:cfg",
			RunID:          "run-1",
			Turn:           3,
			RemainingTurns: 8,
			PendingToolCalls: []checkpoint.PendingToolCall{{
				ID:   "toolu_01ABC",
				Name: "ask_human",
			}},
			ProviderState: json.RawMessage(`{"messages":[]}`),
		},
		Question: "Should I force-push?",
	})
	delay, requeued := workqueue.GetRequeueDelay(err)
	fmt.Println("parked, wake in:", requeued, delay)

	// Unanswered wake: rearm and mutate nothing.
	d, _, err := c.Wake(ctx, "org/repo#42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("unanswered:", d)

	// A human answers through the question store's transport.
	q, ok := questions.Provide(ctx, "org/repo#42", "Yes, go ahead.")
	fmt.Println("question was:", ok, q.Prompt)

	// Answered wake: the envelope is claimed (CAS delete) and the question
	// consumed, exactly once. The caller resumes from the WakeResult; the
	// executor's Resume owns framing the raw answer.
	d, res, err := c.Wake(ctx, "org/repo#42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("answered:", d, res.Answer.Text)

	// The claim removed the checkpoint, so the next wake starts from scratch.
	d, _, err = c.Wake(ctx, "org/repo#42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("after claim:", d)
	// Output:
	// parked, wake in: true 1m0s
	// unanswered: WakeRearm
	// question was: true Should I force-push?
	// answered: WakeResume Yes, go ahead.
	// after claim: WakeFresh
}
