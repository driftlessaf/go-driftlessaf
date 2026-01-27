/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/chainguard-dev/clog"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"golang.org/x/oauth2"
)

const cloneDirPrefix = "clonemanager-clone-"

// repoURL resolves the remote git URL for a githubreconciler.Resource. Tests
// can override this to provide local filesystem paths by assigning a custom
// function to repoURL.
var repoURL = defaultRemoteURL

// Manager owns a pool of git clones that can be leased to callers for a single
// reconciliation. Each lease is dedicated to a GitHub resource and ensures the
// working tree is reset before being returned to the pool.
type Manager struct {
	tokenSource oauth2.TokenSource
	identity    string
	signer      git.Signer

	mu        sync.Mutex
	available []*clone
}

type clone struct {
	path string
	repo *git.Repository
}

// Lease represents an acquired clone prepared for a specific GitHub resource.
// Leases expose convenience accessors for inspecting the checked-out commit and
// a helper for applying and pushing changes.
type Lease struct {
	manager *Manager
	clone   *clone

	sha        string
	pathExists bool
}

// UpdateFunc receives the prepared working tree for a lease and returns the
// commit message that should be used when persisting staged changes.
type UpdateFunc func(context.Context, *git.Worktree) (string, error)

// New constructs a Manager. The provided OAuth2 token source must allow cloning
// and pushing to the targeted repository. Identity is used as the commit author
// name (and, when it lacks a domain, suffixed with @chainguard.dev). The signer
// may be nil when Gitsign-style signing is not required.
func New(_ context.Context, tokenSource oauth2.TokenSource, identity string, signer git.Signer) (*Manager, error) {
	if tokenSource == nil {
		return nil, errors.New("token source cannot be nil")
	}

	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil, errors.New("identity cannot be empty")
	}

	return &Manager{
		tokenSource: tokenSource,
		identity:    identity,
		signer:      signer,
	}, nil
}

// Lease hydrates a clone for the supplied GitHub resource and returns a Lease
// handle. Callers must invoke Return to release the clone back to the pool.
func (m *Manager) Lease(ctx context.Context, res *githubreconciler.Resource) (*Lease, error) {
	switch {
	case res == nil:
		return nil, errors.New("resource cannot be nil")
	case res.Type != githubreconciler.ResourceTypePath:
		return nil, fmt.Errorf("unsupported resource type %q", res.Type)
	case res.Owner == "":
		return nil, errors.New("resource owner cannot be empty")
	case res.Repo == "":
		return nil, errors.New("resource repo cannot be empty")
	case res.Ref == "":
		return nil, errors.New("resource ref cannot be empty")
	case res.Path == "":
		return nil, errors.New("resource path cannot be empty")
	}

	cl, err := m.acquireClone(ctx, res)
	if err != nil {
		return nil, err
	}

	sha, exists, err := m.prepareClone(ctx, cl, res)
	if err != nil {
		clog.FromContext(ctx).Warnf("Discarding clone after prepare failure: %v", err)
		m.discardClone(cl)
		return nil, err
	}

	return &Lease{
		manager:    m,
		clone:      cl,
		sha:        sha,
		pathExists: exists,
	}, nil
}

// acquireClone returns a clone from the pool or creates a new one if the pool
// is empty. Clones are taken from the front of the pool while releaseClone
// appends to the back, so recently returned clones are not immediately reused.
// This prevents problematic clones from churning repeatedly by allowing them
// to age out at the back of the pool.
func (m *Manager) acquireClone(ctx context.Context, res *githubreconciler.Resource) (*clone, error) {
	m.mu.Lock()
	if n := len(m.available); n > 0 {
		cl := m.available[0]
		m.available = m.available[1:]
		m.mu.Unlock()
		return cl, nil
	}
	m.mu.Unlock()

	return m.createClone(ctx, res)
}

func (m *Manager) createClone(ctx context.Context, res *githubreconciler.Resource) (*clone, error) {
	dir, err := os.MkdirTemp("", cloneDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	remote := repoURL(res)
	clog.FromContext(ctx).Infof("Cloning repository %s into %s", remote, dir)

	auth, err := m.authForRemote()
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("getting token: %w", err)
	}

	repo, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL:           remote,
		ReferenceName: plumbing.NewBranchReferenceName(res.Ref),
		SingleBranch:  true,
		Auth:          auth,
	})
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("cloning repository: %w", err)
	}

	return &clone{path: dir, repo: repo}, nil
}

func (m *Manager) prepareClone(ctx context.Context, cl *clone, res *githubreconciler.Resource) (string, bool, error) {
	repo := cl.repo
	if repo == nil {
		var err error
		repo, err = git.PlainOpen(cl.path)
		if err != nil {
			return "", false, fmt.Errorf("opening repo: %w", err)
		}
		cl.repo = repo
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return "", false, fmt.Errorf("getting worktree: %w", err)
	}

	if err := worktree.Reset(&git.ResetOptions{Mode: git.HardReset}); err != nil {
		return "", false, fmt.Errorf("resetting worktree: %w", err)
	}

	if err := worktree.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return "", false, fmt.Errorf("cleaning worktree: %w", err)
	}

	auth, err := m.authForRemote()
	if err != nil {
		return "", false, fmt.Errorf("getting token: %w", err)
	}

	fetchOpts := &git.FetchOptions{
		RefSpecs: []gitconfig.RefSpec{gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", res.Ref, res.Ref))},
		Auth:     auth,
	}

	clog.FromContext(ctx).Infof("Fetching ref %s", res.Ref)
	if err := repo.Fetch(fetchOpts); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", false, fmt.Errorf("fetching ref %s: %w", res.Ref, err)
	}

	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", res.Ref), true)
	if err != nil {
		return "", false, fmt.Errorf("getting remote ref %s: %w", res.Ref, err)
	}

	worktreeCheckout := &git.CheckoutOptions{Hash: remoteRef.Hash(), Force: true}
	if err := worktree.Checkout(worktreeCheckout); err != nil {
		return remoteRef.Hash().String(), false, fmt.Errorf("checking out ref %s: %w", res.Ref, err)
	}

	commit, err := repo.CommitObject(remoteRef.Hash())
	if err != nil {
		return remoteRef.Hash().String(), false, fmt.Errorf("getting commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return remoteRef.Hash().String(), false, fmt.Errorf("getting tree: %w", err)
	}

	// Verify the path exists in the git tree.
	_, err = tree.FindEntry(res.Path)
	if err != nil {
		if errors.Is(err, object.ErrEntryNotFound) {
			clog.FromContext(ctx).Debugf("Path %s not found at commit %s", res.Path, remoteRef.Hash().String())
			return remoteRef.Hash().String(), false, nil
		}
		return remoteRef.Hash().String(), false, fmt.Errorf("checking tree path %s: %w", res.Path, err)
	}

	// Verify the path actually exists on the filesystem, not just in the git tree.
	fsPath := filepath.Join(cl.path, res.Path)
	_, err = os.Stat(fsPath)
	if err != nil {
		if os.IsNotExist(err) {
			clog.FromContext(ctx).Debugf("Path %s does not exist on filesystem at commit %s", res.Path, remoteRef.Hash().String())
			return remoteRef.Hash().String(), false, nil
		}
		return remoteRef.Hash().String(), false, fmt.Errorf("checking fs path %s: %w", res.Path, err)
	}

	clog.FromContext(ctx).Debugf("Path %s exists at commit %s", res.Path, remoteRef.Hash().String())

	status, err := worktree.Status()
	if err != nil {
		return remoteRef.Hash().String(), false, fmt.Errorf("getting worktree status: %w", err)
	}
	if !status.IsClean() {
		return remoteRef.Hash().String(), false, errors.New("worktree is not clean after checkout")
	}

	return remoteRef.Hash().String(), true, nil
}

func (m *Manager) resetClone(cl *clone) error {
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

// releaseClone returns a clone to the back of the pool. Combined with
// acquireClone taking from the front, this prevents churning.
func (m *Manager) releaseClone(cl *clone) {
	m.mu.Lock()
	m.available = append(m.available, cl)
	m.mu.Unlock()
}

func (m *Manager) discardClone(cl *clone) {
	os.RemoveAll(cl.path)
}

func (m *Manager) authForRemote() (*githttp.BasicAuth, error) {
	token, err := m.tokenSource.Token()
	if err != nil {
		return nil, err
	}

	return &githttp.BasicAuth{
		Username: "unused-when-using-access-tokens",
		Password: token.AccessToken,
	}, nil
}

func defaultRemoteURL(res *githubreconciler.Resource) string {
	return fmt.Sprintf("https://github.com/%s/%s", res.Owner, res.Repo)
}

// MakeAndPushChanges creates a new branch at the leased SHA, delegates change
// application to updateFn, commits the staged changes using the manager's
// identity, and force pushes the branch to origin.
func (l *Lease) MakeAndPushChanges(ctx context.Context, branchName string, updateFn UpdateFunc) error {
	if updateFn == nil {
		return errors.New("update function cannot be nil")
	}

	ref, err := l.createFreshBranch(branchName)
	if err != nil {
		return fmt.Errorf("creating fresh branch: %w", err)
	}

	worktree, err := l.clone.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	commitMessage, err := updateFn(ctx, worktree)
	if err != nil {
		return fmt.Errorf("applying updates: %w", err)
	}

	if commitMessage == "" {
		return errors.New("commit message cannot be empty")
	}

	if err := l.manager.commitChanges(l.clone.repo, commitMessage); err != nil {
		return fmt.Errorf("committing changes: %w", err)
	}

	if err := l.manager.forcePushBranch(ctx, l.clone.repo, ref); err != nil {
		return fmt.Errorf("force pushing branch: %w", err)
	}

	return nil
}

func (l *Lease) createFreshBranch(branchName string) (plumbing.ReferenceName, error) {
	if branchName == "" {
		return "", errors.New("branch name cannot be empty")
	}

	refName := plumbing.NewBranchReferenceName(branchName)
	newBranchRef := plumbing.NewHashReference(refName, plumbing.NewHash(l.sha))

	if err := l.clone.repo.Storer.SetReference(newBranchRef); err != nil {
		return "", fmt.Errorf("setting branch reference: %w", err)
	}

	worktree, err := l.clone.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("getting worktree: %w", err)
	}

	if err := worktree.Checkout(&git.CheckoutOptions{Branch: refName, Force: true}); err != nil {
		return "", fmt.Errorf("checking out branch: %w", err)
	}

	return refName, nil
}

func (m *Manager) commitChanges(repo *git.Repository, commitMessage string) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	email := m.identity
	if !strings.Contains(email, "@") {
		email = fmt.Sprintf("%s@chainguard.dev", email)
	}

	_, err = worktree.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  m.identity,
			Email: email,
			When:  time.Now(),
		},
		Signer: m.signer,
	})
	if err != nil {
		return fmt.Errorf("committing: %w", err)
	}

	return nil
}

func (m *Manager) forcePushBranch(ctx context.Context, repo *git.Repository, ref plumbing.ReferenceName) error {
	log := clog.FromContext(ctx)

	token, err := m.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("getting token: %w", err)
	}

	refSpec := gitconfig.RefSpec(fmt.Sprintf("%s:%s", ref.String(), ref.String()))
	log.Infof("Force pushing to %s", refSpec)

	if err := repo.Push(&git.PushOptions{
		RemoteName: "origin",
		Auth: &githttp.BasicAuth{
			Username: "unused-when-using-access-tokens",
			Password: token.AccessToken,
		},
		Force:    true,
		RefSpecs: []gitconfig.RefSpec{refSpec},
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			log.Infof("Branch already up to date")
			return nil
		}
		return fmt.Errorf("force pushing: %w", err)
	}

	return nil
}

// ID returns a clone ID based on the underlying working tree path.
func (l *Lease) ID() string {
	return filepath.Base(l.clone.path)
}

// Repo returns the underlying git repository for this lease.
func (l *Lease) Repo() *git.Repository {
	return l.clone.repo
}

// WorkingTree returns the absolute path to the lease's working directory.
func (l *Lease) WorkingTree() string {
	return l.clone.path
}

// SHA returns the commit hash currently checked out by the lease.
func (l *Lease) SHA() string {
	return l.sha
}

// PathExists reports whether the reconciled resource path exists at the
// checked-out commit.
func (l *Lease) PathExists() bool {
	return l.pathExists
}

// Return resets the working tree and places the clone back into the manager's
// pool. Once Return succeeds, the lease should be considered invalid.
func (l *Lease) Return(ctx context.Context) error {
	if err := l.manager.resetClone(l.clone); err != nil {
		l.manager.discardClone(l.clone)
		l.clone = nil
		return err
	}

	l.manager.releaseClone(l.clone)
	l.clone = nil
	l.manager = nil
	l.sha = ""
	l.pathExists = false

	return nil
}
