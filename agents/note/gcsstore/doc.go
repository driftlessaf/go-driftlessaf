/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package gcsstore implements note.Store over a blob backend — in production a
// Google Cloud Storage bucket — so agents in different processes pass notes
// through durable storage rather than a shared heap.
//
// It composes the store/blob primitive instead of driving the GCS SDK: the
// object CRUD, the prefix listing, and the error mapping live there, and this
// package adds only what is specific to notes — the coordinate layout and the
// mapping from a note.Filter to a prefix scan. It therefore holds no cloud SDK
// import of its own, and runs against blob.Mem in unit tests and against a
// bucket in production without changing.
//
// # Object layout
//
// One object per note, named by its coordinates:
//
//	<root>key_<esc>/run_<N>/name_<esc>/author_<esc>
//
// for example, with root "orgs/example/":
//
//	orgs/example/key_harden%3Awidget/run_1/name_fix_plan/author_claude
//	orgs/example/key_harden%3Awidget/run_1/name_fix_plan/author_          <- the shared scope
//
// The layout is the reordered form of SkillChain2's proven skillstate paths, so
// one prefix scan gathers a step's notes across every author — the common
// fan-in.
//
// # Why the escaping is load-bearing
//
// In [note] a note's identity is [note.Ref], a hash over length-prefixed
// coordinates, which is injective for all inputs. A path-laid-out backend has to
// hold that same property with its names, and a naive layout does not: joining
// coordinates with "/" frames ("a/b", "c") and ("a", "b/c") identically, so one
// note would alias another. Each coordinate is therefore escaped with
// url.PathEscape, which never emits "/" and escapes "%" itself, and each segment
// carries a role prefix so no segment is empty. Encoding is injective and
// decoding re-encodes and compares, which together make name and coordinates a
// bijection: path equality is coordinate equality, hence Ref equality.
//
// An object that is not the canonical name for the coordinates it appears to
// hold — a hand-written "run_01", a lowercase "%2f" — is rejected by List rather
// than reported as a note some other name also maps to. See "Sharing a root"
// below for what a listing does tolerate.
//
// # Bodies are buffered
//
// note.Store passes a body separately from its coordinates (Put takes an
// io.Reader, Get returns an io.ReadCloser) so a note's size is not a property of
// the Note struct. This backend nonetheless holds each body in memory for the
// length of a call, because store/blob is byte-oriented. That is a deliberate
// trade rather than a shortcut:
//
//   - It costs little at note sizes. A storage.Writer allocates a ChunkSize
//     buffer (16 MiB by default) and buffers input into it precisely so a failed
//     request can be retried, so for any note below that size a streaming write
//     allocates the same memory — the SDK simply owns the buffer instead of this
//     package. Removing that buffer means setting ChunkSize to 0, which also
//     disables retry.
//   - It is all-or-nothing for free. A streaming write to GCS commits whatever
//     was written when the writer is closed, so a mid-stream failure has to
//     abort by cancelling the writer's context; closing it publishes a truncated
//     — commonly empty — object that later readers accept as complete. A
//     single-shot byte write either lands whole or errors.
//   - A buffered Get cannot leak. The reader it returns holds no connection, so
//     the list-then-read-each-body fan-in cannot strand open readers.
//
// The ceiling is DefaultMaxNoteSize, overridable per store with WithMaxNoteSize:
// a larger body fails the Put instead of being read into memory without bound. A
// consumer that needs genuinely large notes wants a streaming path in
// store/blob, not an unbounded read here.
//
// # Writes are unconditional
//
// Put overwrites, matching the note.Store contract: the same coordinates always
// name the same object, and the last writer wins. The store/blob primitive
// offers compare-and-swap (Cond, Gen) and this backend deliberately does not use
// it — a note is published once by the step that owns those coordinates, so there
// is no read-modify-write to protect and no lost-update to detect. Get discards
// the object generation for the same reason. A consumer that needs claim-once
// semantics wants a different primitive (agents/checkpoint), not a Cond here.
// Delete is unconditional for the same reason, and reports an absent note as
// note.ErrNotExist rather than succeeding silently.
//
// # Sharing a root
//
// A listing skips objects under the root that are not shaped like note objects,
// so the root may be shared with other data — skillchain keeps a version's notes
// beside its hardened tree and its manifests. What a listing does not tolerate is
// a note-shaped name that is not canonical: that is the aliasing the encoding
// exists to prevent, and it fails the call rather than reporting a note some
// other name also maps to.
//
// Without a Filter.Key, List scans every object under the root, including
// unrelated data. Set Filter.Key when possible to narrow the scan to that key's
// notes, especially when sharing a root with a large hardened tree.
//
// # Tenancy
//
// The root passed to New is the storage boundary, and it is the only boundary
// this package has. Isolation in note.Store is a property of the handle a caller
// holds, never of a coordinate: coordinates are content-addressed so any agent
// can compute a peer's Ref, which is what makes note-passing work and what makes
// a coordinate useless as an authorization check — an agent could simply compute
// another tenant's. So a caller constructs one store per tenant, pinned to an
// org-scoped bucket, project, or prefix, and per-run scoping composes above this
// package rather than inside it.
//
// # Deployment (Terraform, not in this package)
//
// This package writes no Terraform. Deployers should provision a bucket via the
// standard bucket module and pass its name to the service.
//
// Retention is the application's policy, not this store's. A run's notes are its
// durable audit copy, so nothing here expires them: a deployer sets a bucket
// lifecycle rule for the floor, and an application that must remove specific
// notes on request — a customer's, or a run's beyond its retained window —
// walks them with List and removes each with Delete. There is no
// delete-by-prefix, deliberately: a sweep that fails part way through leaves no
// record of how far it got, whereas List-then-Delete leaves each removal a
// separate durable decision.
//
// Conformance with the note.Store contract is asserted by the shared notetest
// suite, run against blob.Mem in this package's tests and against a real bucket
// under the withauth build tag.
package gcsstore
