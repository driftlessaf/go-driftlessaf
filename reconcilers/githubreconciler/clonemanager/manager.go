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
	"github.com/chainguard-dev/terraform-infra-common/pkg/gitexec/gogit"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"golang.org/x/oauth2"
)

const cloneDirPrefix = "clonemanager-clone-"

const gitFetchDepth = 1

// resetTimeout bounds the worktree reset on Return. It runs under
// context.WithoutCancel (so a canceled reconcile does not abort cleanup), which
// also drops the deadline, so a stalled CLI-backend reset/clean would otherwise
// hang forever. Generous versus a normal reset (~sub-second on a 46k-file tree).
const resetTimeout = 2 * time.Minute

// ErrNothingToCommit is returned by MakeAndPushChanges when the update function
// runs without error but leaves the working tree clean (i.e., no diff to commit).
// changemanager.Upsert translates this into changemanager.ErrNoChanges so callers
// can use the standard no-changes protocol.
var ErrNothingToCommit = errors.New("nothing to commit: working tree is clean")

// LeaseOption configures optional parameters for LeaseRef.
type LeaseOption func(*leaseOptions)

type leaseOptions struct {
	depth int
}

// WithCommitDepth sets the fetch depth for the lease. This controls how many
// commits of history are fetched. Use this when you need commit history
// walking (e.g., for list_commits), setting it to the PR commit count + 1
// to include the base commit.
func WithCommitDepth(depth int) LeaseOption {
	return func(o *leaseOptions) {
		o.depth = depth
	}
}

// repoURL resolves the remote git URL for a githubreconciler.Resource. Tests
// can override this to provide local filesystem paths by assigning a custom
// function to repoURL.
var repoURL = defaultRemoteURL

// Option configures a Manager.
type Option func(*Manager)

// WithMaxFetches sets how many pack-transferring fetches a clone serves before
// it is discarded and replaced by a fresh clone. Under the default go-git
// backend every such fetch permanently grows a clone's object store — go-git
// appends a packfile per fetch and never prunes — so on a fast-moving
// repository a pooled clone grows without bound. The bound caps that growth
// at the amortized cost of one re-clone every n fetches. The git CLI backend
// transfers negotiated increments and runs auto maintenance, so there the
// bound is only a backstop. Fetches that find the ref already up to date
// transfer nothing and don't count. Zero or negative (the default) disables
// the bound.
func WithMaxFetches(n int) Option {
	return func(m *Manager) {
		m.maxFetches = n
	}
}

// Manager owns a pool of git clones that can be leased to callers for a single
// reconciliation. Each lease is dedicated to a GitHub resource and ensures the
// working tree is reset before being returned to the pool.
type Manager struct {
	tokenSource oauth2.TokenSource
	identity    string
	signer      git.Signer
	maxFetches  int
	backend     gitBackend

	mu        sync.Mutex
	available []*clone
}

type clone struct {
	path string
	// root confines the manager's own filesystem operations to the clone
	// directory: it is opened once at creation and every direct read/write
	// (config reset, path checks) goes through it, so a symlink planted at
	// any path component by untrusted worktree code cannot redirect an op to
	// an external file. go-git and the git subprocess still use path directly.
	root *os.Root
	repo *git.Repository
	// fetches counts fetches that transferred a pack, i.e. the ones that
	// grew the object store.
	fetches int
}

// newClone wraps a freshly cloned directory, opening the os.Root that confines
// the manager's filesystem operations to it.
func newClone(dir string, repo *git.Repository) (*clone, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening clone root: %w", err)
	}
	return &clone{path: dir, root: root, repo: repo}, nil
}

// Lease represents an acquired clone prepared for a specific GitHub resource.
// Leases expose convenience accessors for inspecting the checked-out commit and
// a helper for applying and pushing changes.
type Lease struct {
	manager *Manager
	clone   *clone

	// remoteURL is the trusted push target, resolved from the GitHub resource
	// when the lease is created (before any untrusted code runs). forcePushBranch
	// pushes here rather than to the clone's on-disk .git/config: a consumer may
	// expose the lease's working tree (which contains .git/) to untrusted code
	// with write access, and a rewritten [remote "origin"] url would otherwise
	// redirect the credentialed push.
	remoteURL string

	sha        string
	pathExists bool
	// baseCommit is the merge-base resolved at lease creation time by
	// walking (fetchDepth - 1) commits from HEAD. This keeps the walk
	// in sync with the actual clone depth so callers never request more
	// history than was fetched.
	baseCommit plumbing.Hash
}

// UpdateFunc receives the prepared working tree for a lease and returns the
// commit message that should be used when persisting staged changes.
type UpdateFunc func(context.Context, *git.Worktree) (string, error)

// New constructs a Manager. The provided OAuth2 token source must allow cloning
// and pushing to the targeted repository. Identity is used as the commit author
// name (and, when it lacks a domain, suffixed with @chainguard.dev). The signer
// may be nil when Gitsign-style signing is not required.
func New(ctx context.Context, tokenSource oauth2.TokenSource, identity string, signer git.Signer, opts ...Option) (*Manager, error) {
	if tokenSource == nil {
		return nil, errors.New("token source cannot be nil")
	}

	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil, errors.New("identity cannot be empty")
	}

	m := &Manager{
		tokenSource: tokenSource,
		identity:    identity,
		signer:      signer,
	}
	m.backend = gogitBackend{tokenSource: tokenSource}
	for _, opt := range opts {
		opt(m)
	}
	if _, ok := m.backend.(cliBackend); ok {
		if err := checkGitVersion(ctx); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Lease hydrates a clone for the supplied GitHub resource and returns a Lease
// handle. For Path resources, it uses res.Ref; for Issue resources, it defaults
// to "main". Callers must invoke Return to release the clone back to the pool.
func (m *Manager) Lease(ctx context.Context, res *githubreconciler.Resource) (*Lease, error) {
	if res == nil {
		return nil, errors.New("resource cannot be nil")
	}

	// Compute default ref based on resource type
	ref := "main"
	if res.Type == githubreconciler.ResourceTypePath {
		if res.Ref == "" {
			return nil, errors.New("resource ref cannot be empty for Path type")
		}
		ref = res.Ref
	}

	return m.leaseRef(ctx, res, ref, gitFetchDepth)
}

// LeaseRef hydrates a clone for the supplied GitHub resource at the specified
// ref and returns a Lease handle. The ref can be a branch name (e.g., "main",
// "feature-branch") that will be fetched and checked out.
// By default it fetches with depth 1. Use WithCommitDepth to fetch deeper
// history for commit walking (e.g., list_commits).
// Callers must invoke Return to release the clone back to the pool.
func (m *Manager) LeaseRef(ctx context.Context, res *githubreconciler.Resource, ref string, opts ...LeaseOption) (*Lease, error) {
	o := leaseOptions{depth: gitFetchDepth}
	for _, opt := range opts {
		opt(&o)
	}
	return m.leaseRef(ctx, res, ref, o.depth)
}

func (m *Manager) leaseRef(ctx context.Context, res *githubreconciler.Resource, ref string, depth int) (*Lease, error) {
	if res == nil {
		return nil, errors.New("resource cannot be nil")
	}
	if ref == "" {
		return nil, errors.New("ref cannot be empty")
	}

	switch res.Type {
	case githubreconciler.ResourceTypePath:
		switch {
		case res.Owner == "":
			return nil, errors.New("resource owner cannot be empty")
		case res.Repo == "":
			return nil, errors.New("resource repo cannot be empty")
		case res.Path == "":
			return nil, errors.New("resource path cannot be empty")
		}
	case githubreconciler.ResourceTypeIssue, githubreconciler.ResourceTypePullRequest:
		switch {
		case res.Owner == "":
			return nil, errors.New("resource owner cannot be empty")
		case res.Repo == "":
			return nil, errors.New("resource repo cannot be empty")
		}
	default:
		return nil, fmt.Errorf("unsupported resource type %q", res.Type)
	}

	cl, err := m.acquireClone(ctx, ref, res)
	if err != nil {
		return nil, err
	}

	sha, exists, err := m.prepareClone(ctx, cl, ref, res, depth)
	if err != nil {
		clog.WarnContextf(ctx, "Discarding clone after prepare failure: %v", err)
		m.discardClone(cl)
		return nil, err
	}

	// Resolve the merge-base eagerly so callers can access it without
	// error handling. The fetch depth includes the base commit
	// (depth = commitCount + 1), so subtract 1 to get the PR commit count.
	baseCommit, err := resolveBaseCommit(cl.repo, max(depth-1, 0))
	if err != nil {
		clog.WarnContextf(ctx, "Discarding clone after base commit resolution failure: %v", err)
		m.discardClone(cl)
		return nil, fmt.Errorf("resolve base commit: %w", err)
	}

	return &Lease{
		manager:    m,
		clone:      cl,
		remoteURL:  repoURL(res),
		sha:        sha,
		pathExists: exists,
		baseCommit: baseCommit,
	}, nil
}

// acquireClone returns a clone from the pool or creates a new one if the pool
// is empty. Clones are taken from the front of the pool while releaseClone
// appends to the back, so recently returned clones are not immediately reused.
// This prevents problematic clones from churning repeatedly by allowing them
// to age out at the back of the pool.
func (m *Manager) acquireClone(ctx context.Context, ref string, res *githubreconciler.Resource) (*clone, error) {
	m.mu.Lock()
	if n := len(m.available); n > 0 {
		cl := m.available[0]
		m.available = m.available[1:]
		m.mu.Unlock()
		return cl, nil
	}
	m.mu.Unlock()

	return m.createClone(ctx, ref, res)
}

func (m *Manager) createClone(ctx context.Context, ref string, res *githubreconciler.Resource) (*clone, error) {
	dir, err := os.MkdirTemp("", cloneDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	remote := repoURL(res)
	clog.InfoContextf(ctx, "Cloning repository %s into %s", remote, dir)

	repo, err := m.backend.clone(ctx, dir, remote, ref)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("cloning repository: %w", err)
	}

	cl, err := newClone(dir, repo)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return cl, nil
}

func (m *Manager) prepareClone(ctx context.Context, cl *clone, ref string, res *githubreconciler.Resource, depth int) (string, bool, error) {
	repo := cl.repo
	if repo == nil {
		var err error
		repo, err = git.PlainOpen(cl.path)
		if err != nil {
			return "", false, fmt.Errorf("opening repo: %w", err)
		}
		cl.repo = repo
	}

	dst := plumbing.NewRemoteReferenceName("origin", ref)
	fetchURL := repoURL(res)
	refspec := fmt.Sprintf("+%s:%s", resolveRefName(ref), dst)

	// Fetch from the trusted URL (see trustedRemote), never from the clone's
	// on-disk .git/config: a pooled clone is reused across reconciles and
	// resetClone does not restore .git/config, so untrusted code that rewrote
	// [remote "origin"] url in an earlier lease could otherwise redirect this
	// credentialed fetch.
	clog.InfoContextf(ctx, "Fetching ref %s", ref)
	grew, err := m.backend.fetch(ctx, cl, fetchURL, refspec, dst, depth)
	if err != nil {
		return "", false, fmt.Errorf("fetching ref %s: %w", ref, err)
	}
	if grew {
		cl.fetches++
	}
	// The backend may have replaced the repository handle (the CLI backend
	// re-opens it after fetching).
	repo = cl.repo

	remoteRef, err := repo.Reference(dst, true)
	if err != nil {
		return "", false, fmt.Errorf("getting remote ref %s: %w", ref, err)
	}
	clog.InfoContext(ctx, "Fetched ref", "ref", ref, "sha", remoteRef.Hash().String())

	headRef, err := repo.Head()
	if err != nil {
		return "", false, fmt.Errorf("getting HEAD ref: %w", err)
	}

	// Skip checkout when HEAD already matches the remote ref: the worktree
	// already contains the correct content.
	if headRef.Hash() != remoteRef.Hash() {
		if err := m.backend.checkout(ctx, cl, remoteRef.Hash()); err != nil {
			return remoteRef.Hash().String(), false, fmt.Errorf("checking out ref %s: %w", ref, err)
		}
	}

	commit, err := repo.CommitObject(remoteRef.Hash())
	if err != nil {
		return remoteRef.Hash().String(), false, fmt.Errorf("getting commit object: %w", err)
	}

	// Only check path existence for Path-type resources
	if res.Type == githubreconciler.ResourceTypePath {
		tree, err := commit.Tree()
		if err != nil {
			return remoteRef.Hash().String(), false, fmt.Errorf("getting tree: %w", err)
		}

		// Verify the path exists in the git tree. FindEntry returns
		// ErrEntryNotFound when the final path component is missing, and
		// ErrDirectoryNotFound when an intermediate directory is missing.
		_, err = tree.FindEntry(res.Path)
		if err != nil {
			if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
				clog.DebugContextf(ctx, "Path %s not found at commit %s", res.Path, remoteRef.Hash().String())
				return remoteRef.Hash().String(), false, nil
			}
			return remoteRef.Hash().String(), false, fmt.Errorf("checking tree path %s: %w", res.Path, err)
		}

		// Verify the path actually exists on the filesystem, not just in the
		// git tree. Stat through the clone's root so the check cannot be
		// redirected outside it by a symlinked path component.
		_, err = cl.root.Stat(res.Path)
		if err != nil {
			if os.IsNotExist(err) {
				clog.DebugContextf(ctx, "Path %s does not exist on filesystem at commit %s", res.Path, remoteRef.Hash().String())
				return remoteRef.Hash().String(), false, nil
			}
			return remoteRef.Hash().String(), false, fmt.Errorf("checking fs path %s: %w", res.Path, err)
		}

		clog.DebugContextf(ctx, "Path %s exists at commit %s", res.Path, remoteRef.Hash().String())
	}

	return remoteRef.Hash().String(), true, nil
}

func (m *Manager) resetClone(ctx context.Context, cl *clone) error {
	if err := m.backend.reset(ctx, cl); err != nil {
		return err
	}
	// reset --hard/clean leave .git/info/exclude (untracked), so a lease could
	// persist ignore rules there across Return. Remove it (git treats absent as
	// empty) through the clone's root: a symlink planted at any path component
	// (.git, .git/info, ...) that escapes the clone is refused rather than
	// followed, so the removal cannot reach an external file. An escaping
	// symlink surfaces as a non-ErrNotExist error and discards the clone, which
	// is the right outcome for a tampered one.
	if err := cl.root.Remove(".git/info/exclude"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing .git/info/exclude: %w", err)
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
	if cl.root != nil {
		cl.root.Close()
	}
	os.RemoveAll(cl.path) //nolint:gosec // G703: our own MkdirTemp path
}

// authForRemote adapts an OAuth2 token source to go-git's BasicAuth shape.
func authForRemote(tokenSource oauth2.TokenSource) (*githttp.BasicAuth, error) {
	token, err := tokenSource.Token()
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

// resolveRefName returns a fully qualified reference name. If ref already
// starts with "refs/" it is used as-is; otherwise it is treated as a branch
// name under refs/heads/.
func resolveRefName(ref string) plumbing.ReferenceName {
	if strings.HasPrefix(ref, "refs/") {
		return plumbing.ReferenceName(ref)
	}
	return plumbing.NewBranchReferenceName(ref)
}

// MakeAndPushChanges creates a new branch at the leased SHA, delegates change
// application to updateFn, commits the staged changes using the manager's
// identity, and force pushes the branch to origin.
//
// The context passed to updateFn carries the worktree (see WithWorktree), so
// code the closure cannot thread the worktree to — result validators, tool
// handlers — can recover it via WorktreeFromContext.
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

	commitMessage, err := updateFn(WithWorktree(ctx, worktree), worktree)
	if err != nil {
		return fmt.Errorf("applying updates: %w", err)
	}

	if commitMessage == "" {
		return errors.New("commit message cannot be empty")
	}

	if err := l.manager.commitChanges(l.clone.repo, commitMessage); err != nil {
		return fmt.Errorf("committing changes: %w", err)
	}

	// Record where the pushed branch sits relative to its merge-base, so a
	// runaway reconcile (a branch that keeps accumulating commits, or that has
	// absorbed a fast-moving base while the PR's base pointer stays frozen at
	// creation) is visible per push rather than having to be reconstructed from
	// the GitHub compare API after the fact. commits_ahead_of_base counts the
	// commits the pushed branch carries above the merge-base resolved at lease
	// time; for a fresh-from-default-branch lease this is the single fix commit,
	// for a PR-branch iteration it is the running PR commit total.
	commitsAheadOfBase := -1
	if commits, cerr := collectCommits(l.clone.repo, l.baseCommit); cerr == nil {
		commitsAheadOfBase = len(commits)
	}
	clog.InfoContext(ctx, "Pushing agent changes",
		"branch", branchName,
		"leased_base_sha", l.sha,
		"merge_base_sha", l.baseCommit.String(),
		"commits_ahead_of_base", commitsAheadOfBase)

	if err := l.manager.forcePushBranch(ctx, l.clone.repo, ref, l.remoteURL); err != nil {
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

	if err := worktree.Checkout(&git.CheckoutOptions{Branch: refName, Keep: true}); err != nil {
		return "", fmt.Errorf("checking out branch: %w", err)
	}

	return refName, nil
}

func (m *Manager) commitChanges(repo *git.Repository, commitMessage string) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("getting worktree status: %w", err)
	}
	if status.IsClean() {
		return ErrNothingToCommit
	}

	// Stage every change in one shot. WorktreeCallbacks deliberately does not
	// stage per write: the executor may run a turn's tool callbacks
	// concurrently, and go-git's Worktree.Add rewrites .git/index non-atomically
	// (truncate in place, no lock), so concurrent Adds tear the index and later
	// reads fail with "invalid checksum" (FUL-411). Staging once here,
	// single-threaded, eliminates the race. AddWithOptions{All:true} is
	// `git add -A` (adds, modifies, removes), writing the index exactly once
	// before commit.
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("staging changes: %w", err)
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

// trustedRemote returns an in-memory git remote pinned to remoteURL, so push and
// fetch resolve their target from it rather than the clone's on-disk .git/config.
// A consumer may expose the lease's working tree (which contains .git/) to
// untrusted code with write access; resolving the target from repo.Config() would
// let a rewritten [remote "origin"] url redirect the operation and leak the access
// token (carried as the BasicAuth password) to an attacker-chosen host.
func trustedRemote(repo *git.Repository, remoteURL string) *gogit.Remote {
	return gogit.NewRemote(repo.Storer, &gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteURL},
	})
}

func (m *Manager) forcePushBranch(ctx context.Context, repo *git.Repository, ref plumbing.ReferenceName, remoteURL string) error {
	if remoteURL == "" {
		return errors.New("refusing to push: no trusted remote URL")
	}

	token, err := m.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("getting token: %w", err)
	}

	refSpec := gitconfig.RefSpec(fmt.Sprintf("%s:%s", ref.String(), ref.String()))
	pushHeadSHA := ""
	if headRef, herr := repo.Reference(ref, true); herr == nil {
		pushHeadSHA = headRef.Hash().String()
	}
	clog.InfoContext(ctx, "Force pushing branch", "ref", ref.String(), "head_sha", pushHeadSHA, "remote", remoteURL)

	remote := trustedRemote(repo, remoteURL)

	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		Auth: &githttp.BasicAuth{
			Username: "unused-when-using-access-tokens",
			Password: token.AccessToken,
		},
		Force:    true,
		RefSpecs: []gitconfig.RefSpec{refSpec},
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		clog.InfoContextf(ctx, "Branch already up to date")
		return nil
	}
	if err != nil {
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

// BaseCommit returns the merge-base resolved at lease creation time.
// For PR branch leases this is the parent of the oldest PR commit;
// for default branch leases (depth 1) this is HEAD, producing empty
// change history.
func (l *Lease) BaseCommit() plumbing.Hash {
	return l.baseCommit
}

// Return resets the working tree and places the clone back into the manager's
// pool. Clones that have served the manager's fetch bound are discarded
// instead, so a replacement is cloned fresh on a later lease. Once Return
// succeeds, the lease should be considered invalid.
func (l *Lease) Return(ctx context.Context) error {
	if max := l.manager.maxFetches; max > 0 && l.clone.fetches >= max {
		clog.InfoContextf(ctx, "Discarding clone %s after %d fetches", filepath.Base(l.clone.path), l.clone.fetches)
		l.manager.discardClone(l.clone)
		l.clone = nil
		l.manager = nil
		l.sha = ""
		l.pathExists = false
		return nil
	}

	// Reset must survive a canceled reconcile: callers defer Return with the
	// reconcile context, and failing here discards the clone — forcing the
	// full re-clone the pool exists to avoid. WithoutCancel drops the deadline
	// too, so bound it: a CLI-backend reset/clean subprocess that stalls (slow
	// disk, NFS) then fails and discards the clone instead of hanging Return.
	resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resetTimeout)
	defer cancel()
	if err := l.manager.resetClone(resetCtx, l.clone); err != nil {
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
