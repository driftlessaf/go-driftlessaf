/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package payloadcrypt_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace/payloadcrypt"
)

// ExampleEncryptor shows the producer (seal) and reader (open) halves against a
// stub DEK-wrap. Production wraps the DEK via Cloud KMS — see kmsseal.New — but
// the seal/open round-trip is otherwise identical and GCP-free.
func ExampleEncryptor() {
	const keyName = "projects/p/locations/us-central1/keyRings/argos/cryptoKeys/agent-trace-payload-key"

	// Stub wrap/unwrap standing in for a KMS Encrypt/Decrypt under the KEK.
	// Real deployments use kmsseal.New for wrap and a PAM-gated KMS Decrypt for
	// unwrap; the DEK never leaves KMS in the clear on the producer side.
	wrap := func(_ context.Context, dek []byte) ([]byte, error) { return dek, nil }
	unwrap := func(_ string, wrapped []byte) ([]byte, error) { return wrapped, nil }

	enc, err := payloadcrypt.New(keyName, wrap)
	if err != nil {
		panic(err)
	}

	// One session per CloudEvent: every field it seals shares a single wrapped
	// DEK, so there is exactly one KMS call per event.
	sess, err := enc.NewSession(context.Background())
	if err != nil {
		panic(err)
	}
	sealed, err := sess.Seal([]byte(`"analyze CVE-2025-1234"`))
	if err != nil {
		panic(err)
	}

	// Reader/break-glass side recovers the plaintext.
	plaintext, err := payloadcrypt.Open(sealed, unwrap)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(plaintext))
	// Output: "analyze CVE-2025-1234"
}
