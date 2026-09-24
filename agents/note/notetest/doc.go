/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package notetest provides a shared conformance suite for note.Store backends.
//
// Every backend — the in-memory [note.Mem] and a durable one such as the GCS
// store alike — is driven through the same [RunConformance] table, so an
// in-memory double cannot silently diverge from the real implementation: the
// moment a backend stops matching the contract, its conformance test fails.
//
// The divergence this guards against is not hypothetical for note.Store,
// because the two backend families reach the same contract by different
// mechanisms. The in-memory store keys a map by [note.Ref] and filters every
// entry in Go; a blob-backed store lays notes out on an object path and gets
// only a leading subset of a filter from its prefix scan. So the suite leans
// hardest on the places where that difference could show: coordinates holding
// path and percent-escape characters (which must not alias one another on a
// path-laid-out backend), the shared [note.Note] Author scope, and paging a
// listing whose pages may be short.
package notetest
