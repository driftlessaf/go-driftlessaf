/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-git/go-git/v5"
	"golang.org/x/oauth2"
)

// TokenSourceForRepo resolves an OAuth2 token source for a given owner/repo pair.
type TokenSourceForRepo func(ctx context.Context, owner, repo string) (oauth2.TokenSource, error)

// Meta manages a cache of Manager instances, one per owner/repo pair.
// It provides thread-safe access to managers and lazily creates them on first use.
type Meta struct {
	ctx            context.Context
	tokenSourceFor TokenSourceForRepo
	identity       string
	signer         git.Signer

	mu       sync.RWMutex
	managers map[string]*Manager
}

// NewMeta creates a Meta that caches Manager instances per owner/repo.
// The tokenSourceFor function is called to obtain credentials when a new
// Manager is needed for a repository.
func NewMeta(ctx context.Context, tokenSourceFor TokenSourceForRepo, identity string, signer git.Signer) *Meta {
	return &Meta{
		ctx:            ctx,
		tokenSourceFor: tokenSourceFor,
		identity:       identity,
		signer:         signer,
		managers:       make(map[string]*Manager),
	}
}

// Get returns a Manager for the given owner/repo, creating one if needed.
// Managers are cached and reused for subsequent calls with the same owner/repo.
func (m *Meta) Get(owner, repo string) (*Manager, error) {
	key := owner + "/" + repo

	// Try to get existing manager
	m.mu.RLock()
	mgr, ok := m.managers[key]
	m.mu.RUnlock()

	if ok {
		return mgr, nil
	}

	// Create new manager
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if mgr, ok := m.managers[key]; ok {
		return mgr, nil
	}

	tokenSource, err := m.tokenSourceFor(m.ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("create token source: %w", err)
	}

	mgr, err = New(m.ctx, tokenSource, m.identity, m.signer)
	if err != nil {
		return nil, fmt.Errorf("create clone manager: %w", err)
	}

	m.managers[key] = mgr
	return mgr, nil
}
