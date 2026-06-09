/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"fmt"
	"sync"

	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/modules/github-bots/sdk"
	"github.com/google/go-github/v84/github"
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
	client = sdk.NewClient(oauth2.NewClient(ctx, tokenSource).Transport)

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
