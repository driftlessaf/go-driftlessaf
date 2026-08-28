/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue_test

import (
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/workqueue"
)

// ExampleDeadLetterError demonstrates a permanent refusal that must surface
// durably: the dispatcher moves the key to the dead-letter queue immediately
// (where Enumerate and drain audits see it) instead of completing it with
// only a logged reason, the way a plain NonRetriableError is dropped.
func ExampleDeadLetterError() {
	err := workqueue.DeadLetterError(
		errors.New("line records several coordinates; an operator must adjudicate"),
		"permanent",
	)

	// The dispatcher checks the dead-letter marker BEFORE the plain
	// non-retriable details, because a DeadLetterError carries both: the
	// NoRetryDetails keep an older dispatcher from retrying it.
	if d := workqueue.GetDeadLetterDetails(err); d != nil {
		fmt.Printf("dead-letter reason: %s\n", d.GetMessage())
	}
	if d := workqueue.GetNonRetriableDetails(err); d != nil {
		fmt.Println("also reads as non-retriable")
	}

	// A plain NonRetriableError carries no dead-letter marker: it drops.
	dropped := workqueue.NonRetriableError(errors.New("stale key"), "retired spelling")
	fmt.Printf("plain non-retriable dead-letters: %v\n", workqueue.GetDeadLetterDetails(dropped) != nil)

	// Output:
	// dead-letter reason: permanent
	// also reads as non-retriable
	// plain non-retriable dead-letters: false
}
