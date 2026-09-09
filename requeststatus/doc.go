/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package requeststatus defines the host-independent request-status model
// shared by manifest-gen, image-gen, and any future reconciler that wants to
// tell a requester and a reviewer what a bot-driven change request is doing.
//
// # Scope
//
// This package defines values and validation only. It does not render an
// [Update] into a [Document], call a host API, or import any GitHub package.
// A separate package per host (e.g. a future githubsurface) implements
// [Surface] and owns everything host-specific: entry identity, encoding,
// transport, and echo suppression. That boundary is enforced by
// TestImportBoundary, not by convention, so a rehosted or additional surface
// changes which adapter is bound and nothing in this package.
//
// # Model
//
// [Phase] is the lifecycle state of a running or stopped request: Running,
// Waiting, NeedsYou, or Failed. [Outcome] is a terminal result that is not a
// failure: NoActionNeeded, Complete, or Canceled. An [Update] carries exactly
// one of Phase or Outcome, refined by an optional [Activity] and attempt
// number, a reason string, and presentation fields. [Validator.Validate]
// rejects an incoherent Update before it reaches a renderer or a Surface.
//
// [Role] names what a [Surface] is for (the requester's surface or the
// reviewer's surface), not what host it runs on. [EntryKey] is the
// host-independent logical identity of one bot-owned status entry; each
// Surface adapter encodes that identity in its own host's terms.
//
// # Reasons
//
// Reason is a free string rather than an enum. NeedsYou and Failed reasons
// are owned by each bot's failure table and stay open: [Validator] only
// requires them to be nonempty. Running and Waiting reasons are owned by
// whatever lifecycle authority classifies them (metareconciler today) and
// are declared at [Validator] construction with [WithReasons]; a Running or
// Waiting reason outside its phase's declared set fails validation. A
// [Validator] built with no declared set for a phase accepts no reason for
// that phase, so a lifecycle authority that forgets to declare its reasons
// fails closed rather than silently accepting anything.
package requeststatus
