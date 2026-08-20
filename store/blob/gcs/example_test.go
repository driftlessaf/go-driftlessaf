/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"cloud.google.com/go/storage"

	"chainguard.dev/driftlessaf/store/blob"
	"chainguard.dev/driftlessaf/store/blob/gcs"
)

// ExampleNew demonstrates a create-only Put against a GCS bucket and detecting
// the "already stored" case via blob.ErrPreconditionFailed.
func ExampleNew() {
	ctx := context.Background()

	client, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatal(err)
	}

	store := gcs.New(client.Bucket("my-notes-bucket"))

	gen, err := store.Put(ctx, "notes/run_1/claude/fix_plan", []byte("the plan"), blob.Cond{DoesNotExist: true})
	switch {
	case errors.Is(err, blob.ErrPreconditionFailed):
		fmt.Println("already stored")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Println("stored at generation", gen)
	}
}
