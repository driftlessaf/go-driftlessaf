/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package kmsseal_test

import (
	"context"
	"log"

	"chainguard.dev/driftlessaf/agents/agenttrace/payloadcrypt/kmsseal"
)

// ExampleNew builds a KMS-backed encryptor for the producer side and hands it to
// the CloudEvent tracer via agenttrace.WithPayloadEncryptor. (Not run: it opens
// a real Cloud KMS client.)
func ExampleNew() {
	ctx := context.Background()

	enc, closeFn, err := kmsseal.New(ctx,
		"projects/p/locations/us-central1/keyRings/argos/cryptoKeys/agent-trace-payload-key")
	if err != nil {
		log.Fatalf("kmsseal.New: %v", err)
	}
	defer func() { _ = closeFn() }()

	// Pass enc to agenttrace.WithPayloadEncryptor(enc) when wiring
	// WithCloudEventEmission so emitted trace/span payloads are sealed.
	_ = enc
}
