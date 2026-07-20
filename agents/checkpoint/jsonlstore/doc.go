/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package jsonlstore provides an append-only, JSONL-file checkpoint.Store for
// local development. Each Save/Delete appends one JSON record; the live state
// is the last record seen per key. On open the file is replayed to rebuild the
// in-memory index and the CAS counter, so state survives process restarts.
//
// The store is safe for concurrent use within a single process, but the log
// file must not be shared between processes. Conformance with the Store
// contract is asserted by the shared storetest suite.
package jsonlstore
