/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package blobtest_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/store/blob"
	"chainguard.dev/driftlessaf/store/blob/blobtest"
)

// Example shows a blob backend satisfying [blobtest.Store] — the method set the
// conformance suite drives — and one operation through it. A backend proves
// conformance from its own test by handing RunConformance a factory:
//
//	func TestConformance(t *testing.T) {
//	    blobtest.RunConformance(t, func() blobtest.Store { return blob.NewMem() })
//	}
func Example() {
	var store blobtest.Store = blob.NewMem() // any backend implements the same method set

	gen, err := store.Put(context.Background(), "notes/run_1/claude/fix_plan", []byte("the plan"), blob.Cond{DoesNotExist: true})
	if err != nil {
		panic(err)
	}
	fmt.Println("stored at generation", gen)
	// Output: stored at generation 1
}
