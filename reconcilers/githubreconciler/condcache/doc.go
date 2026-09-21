/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package condcache makes a GitHub client's GETs conditional: it remembers
// each response's ETag and body, revalidates with If-None-Match, and serves
// the remembered body when GitHub answers 304 Not Modified.
//
// The point is the rate limit. A 304 does not count against the REST budget,
// so a reconciler that re-reads the same resource — the same pull request on
// every event for it, the same check-run listing on every poll — pays for the
// first read and nothing after it until the resource actually changes. It
// buys nothing for a URL that is read once: a resource addressed by commit
// SHA is a new URL each time and never revalidates.
//
// # Why a transport rather than If-None-Match at the call site
//
// go-github's CheckResponse treats any status outside 200-299 as an error, so
// a bare If-None-Match header would surface every 304 to the caller as an
// *github.ErrorResponse. Every call site would have to know to expect it.
// Instead this replays the remembered response as the 200 it was, so callers
// see an ordinary result and need no conditional-request handling at all.
//
// # Scope, and why it is the security property
//
// A Transport's cache is private to that Transport. Two clients
// authenticating as different principals must not share one, because a hit
// serves a remembered body without re-checking authorization: the 304 only
// says the resource is unchanged, not that this caller may read it. Wrapping
// each client's own transport — as githubreconciler's ClientCache does, one
// client per (org, repo) — is what keeps a cached body reachable only by the
// credentials that fetched it.
//
// Token rotation within a client is not a concern: an installation token is
// refreshed hourly but names the same installation, so the principal a cached
// entry belongs to does not change with it.
//
// # Freshness
//
// Nothing is served without asking GitHub. Every read revalidates, and a hit
// means GitHub said the representation is unchanged — so a cached body is
// never stale, and the cache needs no expiry. Cache-Control max-age is
// deliberately ignored: honouring it would serve bodies without revalidating,
// which trades correctness for a saving this package does not need.
package condcache
