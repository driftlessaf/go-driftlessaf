/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chainguard-dev/terraform-infra-common/pkg/gitexec/gogit"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/oauth2"
)

// gitBackend implements the git operations whose implementation differs
// between the go-git library and the git binary: the transport-heavy ones
// (clone, fetch) and the worktree-scan-heavy ones (checkout, reset).
// Everything else (branching, committing, signing, pushing) is shared go-git
// code on Manager.
type gitBackend interface {
	// clone clones remote's ref into dir (single branch, shallow, no tags)
	// and returns a handle to the resulting repository.
	clone(ctx context.Context, dir, remote, ref string) (*git.Repository, error)
	// fetch updates dst in the clone from remote per refspec at the given
	// depth and reports whether the fetch grew the object store, feeding the
	// WithMaxFetches bound. Backends may approximate the signal (the CLI
	// backend undercounts deepening fetches). Implementations may replace
	// cl.repo.
	fetch(ctx context.Context, cl *clone, remote, refspec string, dst plumbing.ReferenceName, depth int) (bool, error)
	// checkout force-checks-out sha, discarding local changes.
	checkout(ctx context.Context, cl *clone, sha plumbing.Hash) error
	// reset returns the working tree to a pristine checkout of HEAD,
	// discarding tracked modifications and untracked files.
	reset(ctx context.Context, cl *clone) error
}

// gogitBackend performs clone, fetch, checkout, and reset through go-git.
type gogitBackend struct {
	tokenSource oauth2.TokenSource
}

func (b gogitBackend) clone(ctx context.Context, dir, remote, ref string) (*git.Repository, error) {
	auth, err := authForRemote(b.tokenSource)
	if err != nil {
		return nil, fmt.Errorf("getting token: %w", err)
	}

	cloneOpts := &git.CloneOptions{
		URL:          remote,
		SingleBranch: true,
		Depth:        gitFetchDepth,
		Auth:         auth,
		// Tag refs pin their objects in the clone forever (nothing ever prunes
		// the object store) and go-git defaults to AllTags, so skip them.
		Tags: git.NoTags,
	}
	// Only set ReferenceName for branch refs. Non-branch refs (e.g.
	// refs/pull/N/head) are not advertised during clone negotiation, so we
	// clone the default branch and let prepareClone fetch the target ref.
	if !strings.HasPrefix(ref, "refs/") {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(ref)
	}
	cloned, err := gogit.PlainCloneContext(ctx, dir, false, cloneOpts)
	if err != nil {
		return nil, err
	}
	return cloned.Repository, nil
}

func (b gogitBackend) fetch(ctx context.Context, cl *clone, remote, refspec string, _ plumbing.ReferenceName, depth int) (bool, error) {
	auth, err := authForRemote(b.tokenSource)
	if err != nil {
		return false, fmt.Errorf("getting token: %w", err)
	}

	fetchOpts := &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(refspec)},
		Auth:       auth,
		Depth:      depth,
		Tags:       git.NoTags,
	}

	switch err := trustedRemote(cl.repo, remote).FetchContext(ctx, fetchOpts); {
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		// No pack transferred; the clone did not grow.
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

func (b gogitBackend) checkout(_ context.Context, cl *clone, sha plumbing.Hash) error {
	worktree, err := cl.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}
	return worktree.Checkout(&git.CheckoutOptions{Hash: sha, Force: true})
}

func (b gogitBackend) reset(_ context.Context, cl *clone) error {
	worktree, err := cl.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	if err := worktree.Reset(&git.ResetOptions{Mode: git.HardReset}); err != nil {
		return fmt.Errorf("resetting worktree: %w", err)
	}

	if err := worktree.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return fmt.Errorf("cleaning worktree: %w", err)
	}

	return nil
}
