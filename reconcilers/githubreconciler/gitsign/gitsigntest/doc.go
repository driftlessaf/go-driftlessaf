/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package gitsigntest provides a local signing authority for testing code
// that signs commits and checks them with [gitsign.VerifyCommit].
//
// A [Fixture] issues its own root and leaf certificates and signs real CMS
// signatures, so certificate-chain and identity checks run unchanged. Only the
// Rekor inclusion check is replaced: the fixture's ledger accepts exactly the
// signatures it produced, and does not model Merkle proofs or signed entry
// timestamps.
package gitsigntest
