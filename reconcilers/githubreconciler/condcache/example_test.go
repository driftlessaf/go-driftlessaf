/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package condcache_test

import (
	"fmt"
	"net/http"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/condcache"
)

// ExampleNew wraps a transport so its GETs revalidate. Reconcilers built on
// githubreconciler should prefer the WithConditionalRequests entrypoint
// option, which does this per client and so keeps each cache scoped to the
// credentials that filled it; construct a Transport directly only when
// wrapping a client you built yourself.
func ExampleNew() {
	rt := condcache.New(http.DefaultTransport,
		condcache.WithMaxEntries(512),
		condcache.WithMaxEntryBytes(32<<10),
	)
	client := &http.Client{Transport: rt}

	fmt.Println("conditional:", client.Transport != nil)
	// Output:
	// conditional: true
}

// ExampleWithMaxEntries shows the knob that turns the cache off: a
// non-positive size leaves the Transport a pass-through, which is the shape
// to reach for when bisecting whether the cache is implicated in a bug.
func ExampleWithMaxEntries() {
	rt := condcache.New(http.DefaultTransport, condcache.WithMaxEntries(0))

	fmt.Println("pass-through:", rt != nil)
	// Output:
	// pass-through: true
}
