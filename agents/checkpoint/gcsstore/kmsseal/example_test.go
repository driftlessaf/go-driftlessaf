/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package kmsseal_test

import (
	"context"

	"cloud.google.com/go/storage"

	"chainguard.dev/driftlessaf/agents/checkpoint/gcsstore"
	"chainguard.dev/driftlessaf/agents/checkpoint/gcsstore/kmsseal"
)

// ExampleNew wires the KMS envelope sealer into a gcsstore.Store, so parked
// envelopes land in GCS as tamper-evident ciphertext readable only with both
// bucket access and KMS Decrypt on the KEK.
func ExampleNew() {
	ctx := context.Background()

	sealer, closeSealer, err := kmsseal.New(ctx,
		"projects/my-project/locations/us-central1/keyRings/my-ring/cryptoKeys/my-kek")
	if err != nil {
		// handle error
		return
	}
	defer closeSealer()

	gcs, err := storage.NewClient(ctx)
	if err != nil {
		// handle error
		return
	}
	defer gcs.Close()

	store := gcsstore.New("my-service", gcs.Bucket("my-checkpoints"), sealer)
	_ = store
}
