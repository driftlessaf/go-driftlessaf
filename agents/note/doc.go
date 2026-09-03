/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package note is the note-passing medium for multi-agent driftlessaf runs.
//
// Different agents don't share memory, so they pass work to each other by
// persisting it: a step Puts a note body, and a downstream step Gets it back by
// the same coordinates. A note's identity is four coordinates — (Key, Run, Name,
// Author) — and its content-addressed primary key is [Ref], a SHA-256 any agent
// can compute from the coordinates alone, so a note is addressable without a
// lookup or an opaque handle.
//
// # The store
//
// [Store] is the persistent read/write medium and the durable audit copy at
// once. This package ships the in-memory backend ([NewMem]) alongside the
// contract — it pulls in no cloud SDK, so it stays here; only a durable backend
// that needs a cloud SDK (GCS) lives in a sibling sub-package. Following "accept
// interfaces, return structs", a backend returns a concrete type and a consumer
// accepts [Store] — or a narrower interface of just the methods it uses.
//
// # Listing
//
// [Store.List] filters by any subset of coordinates and returns a bounded [Page]
// of coordinates; a caller pages by passing Page.Cursor back in Filter.Cursor
// until it is empty. Bodies are streamed on [Store.Get], so a fan-in lists
// coordinates and reads only the bodies it needs.
package note
