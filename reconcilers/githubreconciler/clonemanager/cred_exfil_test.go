/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
)

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
	m := &Manager{tokenSource: staticTokenSource(sentinel), identity: "clonemanager-test"}

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
