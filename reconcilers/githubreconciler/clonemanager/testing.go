/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"golang.org/x/oauth2"
)

// SetRepoURLForTesting overrides the git URL resolver used for clones and
// pushes. Resolvers typically return local fixture paths, so the git CLI
// backend's protocol allowlist is widened to file and http for the duration.
// Returns a restore function; callers should defer or t.Cleanup it.
func SetRepoURLForTesting(fn func(*githubreconciler.Resource) string) func() {
	prevURL, prevProtocols := repoURL, allowedProtocols
	repoURL, allowedProtocols = fn, "https:http:file"
	return func() { repoURL, allowedProtocols = prevURL, prevProtocols }
}

// StaticTokenSource returns an oauth2.TokenSource that always yields the given
// token value.
func StaticTokenSource(token string) oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
}
