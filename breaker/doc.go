/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package breaker provides a keyed circuit breaker for guarding reconcilers
// against remote dependencies returning transient errors.
//
// Transport integrates it as an http.RoundTripper with one circuit per request
// host, surfacing every transient failure as a *Error carrying the backoff to
// wait. Once a host's circuit opens, requests short-circuit without reaching
// the network — granting a single half-open probe per backoff window — so a
// backlog of keys collapses to a trickle of probes instead of hammering an
// unhealthy dependency. The delays suit workqueue.RequeueNotBefore floors.
//
// For non-HTTP dependencies, use a Breaker directly: gate each attempt with
// Allow and report its outcome with RecordSuccess or RecordFailure.
package breaker
