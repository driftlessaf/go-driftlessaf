/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/modules/github-bots/sdk"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

// TokenSourceFunc is a function that creates an OAuth2 token source for a given org/repo.
type TokenSourceFunc func(ctx context.Context, org, repo string) (oauth2.TokenSource, error)

// ClientCache manages GitHub clients for multiple org/repo combinations.
type ClientCache struct {
	tokenSourceFunc TokenSourceFunc
	mu              sync.RWMutex
	clients         map[string]*github.Client
	tokenSources    map[string]oauth2.TokenSource

	// installIDFunc resolves an org to its GitHub App installation ID. It is set
	// by AppMain to the underlying App's (cached) LookupInstallID, so callers can
	// reuse the same installation-ID resolution the token minting already does,
	// rather than constructing a second App. It is nil for non-App entrypoints
	// (e.g. Octo STS via RepoMain/OrgMain), where LookupInstallID returns an error.
	installIDFunc func(ctx context.Context, org string) (int64, error)

	// wrapTransport, when set, wraps each client's authenticated transport
	// before the client is built. Set by WithConditionalRequests; nil leaves
	// the transport as it was.
	//
	// It is applied PER CLIENT, which for a response cache is the whole
	// safety argument: this cache is keyed by (org, repo), so a wrapper that
	// remembers response bodies can only ever serve them back to the
	// credentials that fetched them.
	wrapTransport func(http.RoundTripper) http.RoundTripper
}

// NewClientCache creates a new client cache with the provided token source function.
func NewClientCache(tokenSourceFunc TokenSourceFunc) *ClientCache {
	return &ClientCache{
		tokenSourceFunc: tokenSourceFunc,
		clients:         make(map[string]*github.Client),
		tokenSources:    make(map[string]oauth2.TokenSource),
	}
}

// getKey returns the cache key for an org/repo combination.
func (cc *ClientCache) getKey(org, repo string) string {
	return fmt.Sprintf("%s/%s", org, repo)
}

// LookupInstallID returns the GitHub App installation ID for org, reusing the
// underlying App's cached lookup (the same one used to mint installation
// tokens). It returns an error when the cache was not created from a GitHub App
// entrypoint (AppMain), since only then is an App available to resolve it.
func (cc *ClientCache) LookupInstallID(ctx context.Context, org string) (int64, error) {
	if cc.installIDFunc == nil {
		return 0, fmt.Errorf("installation ID lookup is not configured: the client cache was not created from a GitHub App entrypoint")
	}
	return cc.installIDFunc(ctx, org)
}

// Get returns a GitHub client for the given org/repo, creating one if needed.
func (cc *ClientCache) Get(ctx context.Context, org, repo string) (*github.Client, error) {
	key := cc.getKey(org, repo)

	// Try to get existing client
	cc.mu.RLock()
	client, exists := cc.clients[key]
	cc.mu.RUnlock()

	if exists {
		clog.DebugContext(ctx, "Using cached GitHub client", "org", org, "repo", repo)
		return client, nil
	}

	// Create new client
	cc.mu.Lock()
	defer cc.mu.Unlock()

	// Double-check after acquiring write lock
	if client, exists := cc.clients[key]; exists {
		return client, nil
	}

	tokenSource, err := cc.tokenSourceForLocked(org, repo)
	if err != nil {
		return nil, fmt.Errorf("creating token source: %w", err)
	}

	// Build the client through the SDK primitive so transport instrumentation
	// (httpmetrics) stays consistent with bots constructed via
	// sdk.NewGitHubClient / sdk.NewInstallationClient.
	//
	// Any wrapper sits BELOW httpmetrics, which sdk.NewClient puts outermost.
	// A conditional-request cache therefore still shows up as a request in
	// the metrics and the access log, which is correct: the call is made, it
	// just answers 304 and costs no quota.
	transport := oauth2.NewClient(ctx, tokenSource).Transport
	if cc.wrapTransport != nil {
		transport = cc.wrapTransport(transport)
	}
	client = sdk.NewClient(transport)

	// Cache the client
	cc.clients[key] = client

	clog.InfoContext(ctx, "Created new GitHub client for repository", "org", org, "repo", repo)

	return client, nil
}

// TokenSourceFor returns an OAuth2 token source for the given org/repo combination.
// This allows callers that need raw token sources (e.g., for git clone operations)
// to reuse the same token source function that backs the client cache.
func (cc *ClientCache) TokenSourceFor(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
	key := cc.getKey(org, repo)

	cc.mu.RLock()
	tokenSource, exists := cc.tokenSources[key]
	cc.mu.RUnlock()

	if exists {
		clog.DebugContext(ctx, "Using cached GitHub token source", "org", org, "repo", repo)
		return tokenSource, nil
	}

	cc.mu.Lock()
	defer cc.mu.Unlock()

	if tokenSource, exists = cc.tokenSources[key]; exists {
		return tokenSource, nil
	}

	tokenSource, err := cc.tokenSourceForLocked(org, repo)
	if err != nil {
		return nil, err
	}

	clog.InfoContext(ctx, "Created new GitHub token source for repository", "org", org, "repo", repo)

	return tokenSource, nil
}

// tokenSourceForLocked returns the cached token source for org/repo.
// Callers must hold cc.mu as a write lock.
func (cc *ClientCache) tokenSourceForLocked(org, repo string) (oauth2.TokenSource, error) {
	key := cc.getKey(org, repo)
	if tokenSource, exists := cc.tokenSources[key]; exists {
		return tokenSource, nil
	}

	// Use context.Background() because token sources capture the context for
	// later Token refreshes. Binding a cached source to a request context can
	// poison the cache after that request is canceled, and can leak request-
	// scoped deadlines or values into later refreshes.
	tokenSource, err := cc.tokenSourceFunc(context.Background(), org, repo)
	if err != nil {
		return nil, err
	}

	cc.tokenSources[key] = tokenSource
	return tokenSource, nil
}

// Clear removes all cached clients and token sources.
func (cc *ClientCache) Clear() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.clients = make(map[string]*github.Client)
	cc.tokenSources = make(map[string]oauth2.TokenSource)
}
