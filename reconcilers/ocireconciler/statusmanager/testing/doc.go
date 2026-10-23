/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package testing provides local signing services for statusmanager tests.
// NewLocal supplies Fulcio certificates with signed certificate timestamps,
// Rekor v2 inclusion evidence and RFC3161 timestamps from test-owned keys.
// Managers use their production signer and verifier with these local roots.
//
// With the withauth build tag, New and NewReadOnly use chainctl authentication
// and public Sigstore services to test the production identity/trust contract.
package testing
