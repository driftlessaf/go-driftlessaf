/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus

import "context"

// Document is the content contract between the model and a [Surface]
// adapter: what a status entry says, per audience. It carries no identity,
// no host encoding, and no timestamp. Omitting a rendered update time keeps
// two renders of an unchanged [Update] identical, which is what lets
// [Surface.Publish] skip a redundant write; a rendered timestamp would make
// every publish a distinct body and turn idempotent republication into an
// unbounded write loop.
//
// The fields that make up a Document's audience-specific content (headline,
// summary, step list, and so on) are added by the change that builds an
// Update -> Document renderer; this package defines the contract's
// properties, not its rendered content.
type Document struct{}

// Surface is one place a projection publishes a status entry, identified by
// audience [Role] rather than by host: the requester's surface, or the
// reviewer's surface. A concrete adapter (for example a future
// githubsurface) is the only place a host name appears; this package knows
// nothing about any host.
//
// Implementations must:
//
//   - Upsert exactly one entry per EntryKey: create it on the first
//     Publish for that key, and edit it in place on every later call for
//     the same key.
//   - Perform no write when the Document to publish is unchanged from the
//     last Document this Surface wrote for that key. Request status is a
//     write-only projection; a Surface that writes on every call turns a
//     benign echo (its own write triggering reconciliation, on a host
//     where the surface is also a reconciliation input) into an unbounded
//     loop.
//
// RemoveSuperseded deletes, best effort, any entries at the given keys. It
// exists for legacy-comment cleanup and its failure must not fail a
// concurrent or subsequent Publish. Callers should validate keys with
// [ValidateSupersededKeys] before calling RemoveSuperseded, since a
// too-broad key deletes entries this package's Publish contract requires a
// Surface to preserve.
type Surface interface {
	Publish(ctx context.Context, key EntryKey, doc Document) error
	RemoveSuperseded(ctx context.Context, keys ...EntryKey) error
}
