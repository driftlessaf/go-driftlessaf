/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package blob is a small content-store primitive: named objects with
// compare-and-swap writes, generation-conditional deletes, and prefix listing,
// over a pluggable backend.
//
// It exists so the several driftlessaf stores that keep state in object storage
// stop re-implementing the same GCS plumbing — conditional writes, generation
// tracking, prefix scans, and error mapping. A caller composes a blob backend
// and layers its own semantics (content-addressed keys, sealed envelopes, queue
// state) on top.
//
// # The backend method set
//
// This package does not export a backend interface. Following Go's "accept
// interfaces, return structs", each backend returns a concrete struct and every
// consumer declares the small interface it actually needs. Both [Mem] (here)
// and the GCS backend in the gcs subpackage provide the same method set:
//
//	Put(ctx, name string, data []byte, cond Cond) (Gen, error)
//	Get(ctx, name string) (data []byte, gen Gen, ok bool, err error)
//	Delete(ctx, name string, ifGen Gen) error
//	List(ctx, prefix string, limit int, cursor string) (Page, error)
//
// A consumer that only reads and writes, for example, declares just the Put and
// Get methods; the concrete backend satisfies it implicitly.
//
// # Compare-and-swap
//
// Writes carry an optional [Cond]: DoesNotExist makes a Put create-only (an
// existing object fails with [ErrPreconditionFailed] — how idempotent,
// content-addressed callers detect "already stored"), and GenerationMatch makes
// it a read-modify-write against a known [Gen]. Delete is unconditional when
// ifGen is zero and generation-conditional otherwise. Every write advances the
// object's [Gen], invalidating generations captured before it.
//
// # Errors
//
// Backends map their native failures onto [ErrNotExist] and
// [ErrPreconditionFailed] so callers handle them uniformly with errors.Is
// regardless of backend. Other errors pass through unchanged so callers can
// retry transient failures.
//
// # Listing
//
// List returns object names and generations only — never bodies. It is bounded:
// it returns at most its limit (a backend default when limit <= 0) as a [Page],
// and a caller pages a wide prefix via Page.Cursor rather than accumulating an
// unbounded slice. Callers fetch bodies lazily with Get.
//
// # Names
//
// Object names are opaque keys in a flat namespace; blob does no key validation
// and there is no reserved prefix to escape (unlike a store that namespaces keys
// under an identity). A caller that derives names from untrusted input owns
// sanitizing and namespacing them.
package blob
