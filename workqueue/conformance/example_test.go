/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package conformance_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/conformance"
	"chainguard.dev/driftlessaf/workqueue/inmem"
)

// ExampleExpectedState demonstrates constructing an ExpectedState value used
// by the conformance suite to assert queue contents.
func ExampleExpectedState() {
	es := conformance.ExpectedState{
		WorkInProgress: []string{"key-a"},
		Queued:         []string{"key-b", "key-c"},
	}
	fmt.Printf("wip=%d queued=%d\n", len(es.WorkInProgress), len(es.Queued))
	// Output: wip=1 queued=2
}

func ExampleNewWorkQueue_conformance() {
	ctor := func(limit int) workqueue.Interface {
		return inmem.NewWorkQueue(limit)
	}
	wq := ctor(5)
	fmt.Printf("workqueue created: %v\n", wq != nil)
	// Output: workqueue created: true
}
