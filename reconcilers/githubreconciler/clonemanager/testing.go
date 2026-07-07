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
// pushes. Returns a restore function; callers should defer or t.Cleanup it.
func SetRepoURLForTesting(fn func(*githubreconciler.Resource) string) func() {
	prev := repoURL
	repoURL = fn
	return func() { repoURL = prev }
}

// StaticTokenSource returns an oauth2.TokenSource that always yields the given
// token value.
func StaticTokenSource(token string) oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
}
