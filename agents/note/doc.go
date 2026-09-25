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
// The durable backend is agents/note/gcsstore, which lays notes out on object
// names over the store/blob primitive. Every backend is held to one contract by
// agents/note/notetest, the shared conformance suite [Mem] also runs, so the
// in-memory store cannot drift from the durable one it stands in for.
//
// # Listing
//
// [Store.List] filters by any subset of coordinates and returns a bounded [Page]
// of coordinates; a caller pages by passing Page.Cursor back in Filter.Cursor
// until it is empty. Bodies are fetched separately on [Store.Get], so a fan-in
// lists coordinates and reads only the bodies it needs.
//
// A backend narrows a [Filter] as far as its storage can — a blob prefix covers
// a leading subset of the coordinates — and applies the rest with
// [Filter.Matches], so the selection rule is defined once rather than per
// backend. That is why a page can be short while Page.Cursor is live: page until
// the cursor is empty, never until a page looks small.
//
// # Scoping
//
// Isolation is a capability, not a coordinate. A note's coordinates are
// content-addressed precisely so any agent can compute a peer's [Ref] without a
// lookup — which is what makes note-passing work, and what makes a coordinate
// useless as an authorization check, since an agent could compute another
// scope's just as easily. So the boundary is the handle a caller holds:
// [NewScoped] confines a Store to one [Scope], and the scope is fixed when the
// handle is minted.
//
// The grant ladder runs from narrowest outward. A Scope with a Run is what one
// attempt's agents share; a Scope with only a Key is the broader grant an
// orchestrator needs to compare attempts (retry, a rag layer embedding past
// runs, a synthesizer); the unscoped store is wider still. Above all of them
// sits the store's own namespace, holding the axes a handle cannot cross at all
// — the per-tenant floor, and the separation between pipelines kept in
// different stores — which is why that namespace is pinned when a backend is
// constructed rather than passed per call.
package note
