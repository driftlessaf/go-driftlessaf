/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"cloud.google.com/go/storage"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/gcsstore"
)

// ExampleNew demonstrates parking a suspended envelope in GCS and claiming it
// back with the Load token. IdentitySealer stores envelopes unsealed and is
// for tests and local dev only; production passes a KMS-envelope Sealer.
func ExampleNew() {
	ctx := context.Background()

	client, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatal(err)
	}
	bucket := client.Bucket("my-checkpoint-bucket")

	store := gcsstore.New("my-agent", bucket, gcsstore.IdentitySealer{})

	env := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		Provider:      checkpoint.ProviderAnthropic,
		ReconcilerKey: "org/repo#42",
		RunID:         "run-1",
		ProviderState: json.RawMessage(`{"model":"claude-fable-5"}`),
	}
	if err := store.Save(ctx, env.ReconcilerKey, env); err != nil {
		log.Fatal(err)
	}

	loaded, tok, ok, err := store.Load(ctx, env.ReconcilerKey)
	if err != nil {
		log.Fatal(err)
	}
	if !ok {
		fmt.Println("no envelope parked")
		return
	}
	fmt.Println("parked:", loaded.RunID)

	// Delete with the Load token claims the envelope exactly once; a
	// concurrent waker holding a stale token loses with ErrTokenMismatch.
	if err := store.Delete(ctx, env.ReconcilerKey, tok); err != nil {
		log.Fatal(err)
	}
}
