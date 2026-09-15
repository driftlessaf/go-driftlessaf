/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/chainguard-dev/terraform-infra-common/pkg/gitexec"
	"github.com/chainguard-dev/terraform-infra-common/pkg/gitexec/gitenv"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/oauth2"
)

// allowedProtocols is the GIT_ALLOW_PROTOCOL value for CLI git invocations.
// The manager only ever constructs https URLs, so production pins https alone:
// this stops a url.*.insteadOf rewrite planted in .git/config from redirecting
// a fetch to file:// (attacker-chosen local objects), ssh, ext, or another
// remote helper. Tests override it to add http/file for their local fixtures.
var allowedProtocols = "https"

// WithGitCLI makes the Manager shell out to the git binary for clones,
// fetches, checkouts, and worktree resets instead of using go-git. A shallow
// go-git fetch cannot negotiate against the commits it already has, so on a
// moving branch every fetch transfers a full snapshot pack, and go-git's
// checkout and reset walk the entire working tree where native git leans on
// the index stat cache (measured ~10x on a 46k-file tree). All other
// operations (branching, committing, signing, pushing) still go through
// go-git. Requires git >= gitenv.MinMajor.MinMinor on PATH; New fails
// otherwise.
func WithGitCLI() Option {
	return func(m *Manager) {
		m.backend = cliBackend{tokenSource: m.tokenSource}
	}
}

// cliBackend performs clone, fetch, checkout, and reset by shelling out to
// the git binary through gitexec.
//
// Config-poisoning invariant: a pooled clone's .git/ may have been written by
// untrusted code in a prior lease (e.g. a consumer that mounted the worktree
// read-write into a microvm), and .git/config is the git CLI's redirect and
// code-execution surface (url.insteadOf, http.proxy, core.sshCommand,
// credential.helper, filter.*.smudge, hooks, ...). Rather than pin each such
// key, runInClone calls resetGitConfig to rewrite .git/config to a minimal
// template before every in-clone git op, so no matter which key was planted it
// is gone before git reads it; core.hooksPath and GIT_ALLOW_PROTOCOL in the
// env close the surfaces .git/config alone does not. The only residual is a
// re-plant in the window between that wipe and git's read, which needs a
// concurrent writer in the same clone — precluded by exclusive leases. New
// findings naming another config key are covered by this invariant, not a new
// pin.
type cliBackend struct {
	tokenSource oauth2.TokenSource
}

func (b cliBackend) clone(ctx context.Context, dir, remote, ref string) (*git.Repository, error) {
	args := []string{"clone", "--quiet", "--single-branch", "--no-tags", fmt.Sprintf("--depth=%d", gitFetchDepth)}
	// Only pass --branch for branch refs. Non-branch refs (e.g.
	// refs/pull/N/head) are not advertised during clone negotiation, so we
	// clone the default branch and let prepareClone fetch the target ref.
	if !strings.HasPrefix(ref, "refs/") {
		args = append(args, "--branch", ref)
	}
	args = append(args, remote, dir)

	if err := b.run(ctx, dir, remote, args...); err != nil {
		return nil, err
	}
	return git.PlainOpen(dir)
}

func (b cliBackend) fetch(ctx context.Context, cl *clone, remote, refspec string, dst plumbing.ReferenceName, depth int) (bool, error) {
	before := remoteRefHash(cl.repo, dst)

	if err := b.runInClone(ctx, cl, remote, "fetch", "--quiet", "--no-tags", fmt.Sprintf("--depth=%d", depth), remote, refspec); err != nil {
		return false, err
	}

	// Re-open the repository: go-git caches the packfile list, so objects a
	// separate git process added can be invisible to the open handle.
	repo, err := git.PlainOpen(cl.path)
	if err != nil {
		return false, fmt.Errorf("reopening repo after fetch: %w", err)
	}
	cl.repo = repo

	// Ref movement approximates go-git's NoErrAlreadyUpToDate signal. It
	// undercounts a deepening fetch (same tip, larger --depth), which is
	// acceptable: the fetch bound exists for go-git's append-only pack
	// growth, while native git transfers negotiated increments and runs
	// auto maintenance, so the bound is only a backstop here.
	return remoteRefHash(repo, dst) != before, nil
}

func (b cliBackend) checkout(ctx context.Context, cl *clone, sha plumbing.Hash) error {
	return b.runInClone(ctx, cl, "", "checkout", "--quiet", "--force", "--detach", sha.String())
}

func (b cliBackend) reset(ctx context.Context, cl *clone) error {
	if err := b.runInClone(ctx, cl, "", "reset", "--quiet", "--hard"); err != nil {
		return err
	}
	// -x removes ignored and .git/info/exclude'd files to match go-git's
	// reset+clean: without it an exclude entry planted by untrusted code
	// would keep its payload alive across leases, to be swept up by the
	// stage-everything commit of a later lease. Neither backend removes
	// untracked nested git repositories (native git needs -ff for that).
	return b.runInClone(ctx, cl, "", "clean", "--quiet", "-fdx")
}

// runInClone runs git inside the clone's working tree, resetting the clone's
// .git/config first (see resetGitConfig). The environment — whose token
// acquisition may take a network round-trip — is built before the reset, so
// the window between resetting the config and git reading it stays as small
// as the process spawn itself.
func (b cliBackend) runInClone(ctx context.Context, cl *clone, remote string, args ...string) error {
	env, err := b.env(remote)
	if err != nil {
		return err
	}
	if err := resetGitConfig(cl.root); err != nil {
		return fmt.Errorf("resetting git config: %w", err)
	}
	return b.exec(ctx, cl.path, remote, env, args...)
}

// run executes git with the given args. The subcommand (args[0]) is the
// gitexec op label. dir is the working directory for every op except clone,
// where cmd.Dir stays unset and dir is the pre-created empty target passed
// as an absolute path in args. remote is non-empty exactly for the network
// ops (clone, fetch); only those get the access token in their environment.
func (b cliBackend) run(ctx context.Context, dir, remote string, args ...string) error {
	env, err := b.env(remote)
	if err != nil {
		return err
	}
	return b.exec(ctx, dir, remote, env, args...)
}

func (b cliBackend) exec(ctx context.Context, dir, remote string, env []string, args ...string) error {
	cmd := gitexec.CommandContext(ctx, args...)
	if args[0] != "clone" {
		cmd.Dir = dir
	}
	cmd.Env = env

	var opts []gitexec.Option
	if remote != "" {
		opts = append(opts, gitexec.WithRepoURL(remote))
	}
	return gitexec.Run(ctx, args[0], cmd, opts...)
}

// env returns the environment for a git CLI invocation (gitenv.Environ):
// the parent environment with system and global git config disabled (only
// command-line flags, config-over-environment, and the clone's own
// .git/config apply), prompting disabled, transport protocols pinned to
// allowedProtocols, and hooks neutralized. remote is the network URL for
// clone/fetch and empty for purely local git runs, which then carry no
// credential at all and cannot fail on token refresh. The command-scope
// pins outrank anything in .git/config, including a config re-poisoned by
// a still-running process AFTER resetGitConfig ran; a longer planted URL
// prefix could still win a url.insteadOf tie, but resetGitConfig wipes it
// first and GIT_ALLOW_PROTOCOL bounds where a survivor could point.
func (b cliBackend) env(remote string) ([]string, error) {
	return gitenv.Environ(gitenv.Options{
		Remote:           remote,
		TokenSource:      b.tokenSource,
		AllowedProtocols: allowedProtocols,
	})
}

// remoteRefHash resolves name in repo, returning plumbing.ZeroHash when the
// reference does not exist (e.g. before the first fetch of a ref).
func remoteRefHash(repo *git.Repository, name plumbing.ReferenceName) plumbing.Hash {
	ref, err := repo.Reference(name, true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// minimalGitConfig is the entire .git/config a pooled clone needs on the
// Linux filesystems this runs on (macOS git additionally probes
// core.ignorecase/precomposeunicode at clone time): fetches name the remote
// URL explicitly, so no [remote] section is required, and anything else is
// unwanted (see resetGitConfig).
const minimalGitConfig = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	logallrefupdates = true
`

// resetGitConfig replaces the clone's .git/config with minimalGitConfig,
// enforcing the config-poisoning invariant documented on cliBackend. It is the
// CLI counterpart of trustedRemote (which protects the go-git path).
//
// Every path goes through the clone's os.Root, so a symlink planted at .git or
// any component that escapes the clone is refused rather than followed. The
// replacement is write-to-temp plus rename: rename replaces whatever inode sits
// at .git/config (symlink, FIFO, regular) without opening it. The temp is
// removed first (unlinking a symlink/FIFO planted in its place, without
// following or blocking) then created O_EXCL; leases are exclusive and a
// clone's git ops run serially, so nothing races on the fixed temp name.
func resetGitConfig(root *os.Root) error {
	const tmpName = ".git/config.new"
	if err := root.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing temp config: %w", err)
	}
	f, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating config: %w", err)
	}
	_, werr := f.WriteString(minimalGitConfig)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = root.Remove(tmpName)
		return fmt.Errorf("writing config: %w", cmp.Or(werr, cerr))
	}
	return root.Rename(tmpName, ".git/config")
}
