/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package gitsign signs git commits with Sigstore keyless signing and
// verifies those signatures.
//
// NewSigner uses Fulcio for certificate issuance and Rekor for transparency
// log inclusion, obtaining an OIDC token from the ambient Google Cloud
// credential provider (e.g., a Cloud Run service account).
//
// VerifyCommit checks that a commit was signed by an expected Google service
// account: the certificate must chain to the public-good Fulcio roots, name
// that account and the Google issuer, and carry a verified Rekor inclusion
// receipt from within its validity window. The gitsigntest package provides
// a local signing authority for tests.
package gitsign
