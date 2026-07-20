/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package memstore provides an in-memory checkpoint.Store for tests and
// single-process demos. CAS tokens are minted from a per-store monotonic
// counter, and Save/Load hand back deep copies so stored envelopes are safe
// against caller mutation.
//
// State does not survive a process restart; use jsonlstore for local
// durability. Conformance with the Store contract is asserted by the shared
// storetest suite.
package memstore
