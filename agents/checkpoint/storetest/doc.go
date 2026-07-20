/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package storetest provides a reusable conformance suite for checkpoint.Store
// implementations, in the style of workqueue/conformance. Every Store (memstore,
// jsonlstore, and the future GCS store) is expected to pass RunConformance.
//
// The suite exercises the full Store contract: Load-miss behavior, Save/Load
// round-trips (byte-for-byte, deep-copied), claim-once Delete via CAS tokens,
// token mismatch on stale or absent tokens, and re-save invalidating an old
// token.
package storetest
