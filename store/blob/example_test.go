/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package blob_test

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/store/blob"
)

// ExampleMem demonstrates the blob backend contract against the in-memory
// implementation: a create-only Put, an idempotent re-Put that reports the
// object already exists, a Get, and a prefix List.
func ExampleMem() {
	ctx := context.Background()
	store := blob.NewMem()

	// Create-only write.
	gen, err := store.Put(ctx, "notes/run_1/claude/fix_plan", []byte("the plan"), blob.Cond{DoesNotExist: true})
	if err != nil {
		panic(err)
	}
	fmt.Println("stored at generation:", gen)

	// A second create-only write of the same object is rejected — the signal an
	// idempotent, content-addressed caller treats as "already stored".
	if _, err := store.Put(ctx, "notes/run_1/claude/fix_plan", []byte("again"), blob.Cond{DoesNotExist: true}); errors.Is(err, blob.ErrPreconditionFailed) {
		fmt.Println("already stored")
	}

	data, _, ok, err := store.Get(ctx, "notes/run_1/claude/fix_plan")
	if err != nil {
		panic(err)
	}
	fmt.Printf("get ok=%t body=%q\n", ok, data)

	page, err := store.List(ctx, "notes/run_1/", 0, "")
	if err != nil {
		panic(err)
	}
	fmt.Println("listed:", len(page.Objects), page.Objects[0].Name)

	// Output:
	// stored at generation: 1
	// already stored
	// get ok=true body="the plan"
	// listed: 1 notes/run_1/claude/fix_plan
}
