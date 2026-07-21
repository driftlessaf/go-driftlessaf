/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubquestions_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/agents/suspend/githubquestions"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
)

// ExampleNew demonstrates the question lifecycle on a PR conversation: the
// coordinator posts the question as a comment, polls for a collaborator's
// "/answer <text>" reply, and retires the comment once the answer is
// consumed.
func ExampleNew() {
	ctx := context.Background()

	// The same OctoSTS-backed client cache the reconciler uses.
	clients := githubreconciler.NewClientCache(nil)
	questions := githubquestions.New(clients)

	const key = "org/repo#42" // githubreconciler PR/issue key form
	if err := questions.Ask(ctx, key, suspend.Question{
		ID:      "pause-nonce-1",
		Key:     key,
		Prompt:  "Deploy to staging or production?",
		AskedAt: time.Now().UTC(),
	}); err != nil {
		log.Fatal(err)
	}

	if q, ok, err := questions.Pending(ctx, key); err != nil {
		log.Fatal(err)
	} else if ok {
		fmt.Println("pending:", q.Prompt)
	}

	// A collaborator replies "/answer staging" on the PR; the next poll
	// surfaces it, bound to the pending question's nonce.
	ans, ok, err := questions.Answer(ctx, key)
	if err != nil {
		log.Fatal(err)
	}
	if ok {
		fmt.Println("resume with:", ans.Text)
		if err := questions.Consume(ctx, key, ans.QuestionID); err != nil {
			log.Fatal(err)
		}
	}
}
