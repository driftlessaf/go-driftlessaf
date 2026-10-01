/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

// waitResult waits for a result a test needs to arrive, failing with a
// diagnostic instead of hanging the package when it does not.
func waitResult[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

type getResult struct {
	client *github.Client
	err    error
}

// goGet runs cc.Get(ctx, org, "repo") in the background.
func goGet(ctx context.Context, cc *ClientCache, org string) <-chan getResult {
	ch := make(chan getResult, 1)
	go func() {
		c, err := cc.Get(ctx, org, "repo")
		ch <- getResult{c, err}
	}()
	return ch
}

type lookupResult struct {
	id  int64
	err error
}

func goLookup(ctx context.Context, app *App, org string) <-chan lookupResult {
	ch := make(chan lookupResult, 1)
	go func() {
		id, err := app.LookupInstallID(ctx, org)
		ch <- lookupResult{id, err}
	}()
	return ch
}

// A slow installation lookup for one org must not hold up a Get for another.
// Before the fix every creation ran under the cache-wide write lock, so the
// fast org's Get parked behind the slow org's lookup and this test hung.
func TestClientCache_SlowLookupDoesNotBlockOtherOrgs(t *testing.T) {
	fake := newFakeGitHub(map[string]int64{"fast": 1, "slow": 2})
	release := make(chan struct{})
	fake.block["slow"] = release
	cc := newTestAppCache(t, fake)
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(closeRelease) // before the server closes, if a check fails
	ctx := t.Context()

	slow := goGet(ctx, cc, "slow")
	if got := waitResult(t, fake.entered, "slow lookup to park"); got != "slow" {
		t.Fatalf("parked owner: got = %q, want = %q", got, "slow")
	}

	fast := waitResult(t, goGet(ctx, cc, "fast"), "fast Get while slow lookup is parked")
	if fast.err != nil {
		t.Fatalf("fast Get: %v", fast.err)
	}
	select {
	case r := <-slow:
		t.Fatalf("slow Get returned while its lookup was parked: %v", r.err)
	default:
	}

	// The fast org's client is usable while the slow lookup is still parked.
	if _, _, err := fast.client.Repositories.Get(ctx, "fast", "repo"); err != nil {
		t.Fatalf("fast client: %v", err)
	}

	closeRelease()
	if r := waitResult(t, slow, "slow Get after release"); r.err != nil {
		t.Fatalf("slow Get: %v", r.err)
	}
}

// Concurrent Gets for one new org share one lookup, and a caller that gives
// up gets its own ctx error without failing the others.
func TestClientCache_CallerCancelLeavesSharedLookupRunning(t *testing.T) {
	fake := newFakeGitHub(map[string]int64{"acme": 2})
	release := make(chan struct{})
	fake.block["acme"] = release
	cc := newTestAppCache(t, fake)
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(closeRelease)

	cancelCtx, cancel := context.WithCancel(t.Context())
	quitter := goGet(cancelCtx, cc, "acme")
	waitResult(t, fake.entered, "lookup to park")
	stayer := goGet(t.Context(), cc, "acme")

	cancel()
	if r := waitResult(t, quitter, "cancelled Get"); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled Get: got = %v, want = %v", r.err, context.Canceled)
	}

	closeRelease()
	if r := waitResult(t, stayer, "remaining Get"); r.err != nil {
		t.Fatalf("remaining Get: %v", r.err)
	}
	if got := fake.lookups("acme"); got != 1 {
		t.Errorf("lookups: got = %d, want = 1 (one shared lookup)", got)
	}
}

// The same at the App: the caller that started a lookup cancelling returns
// its own ctx error at once (the ctx.Done wait), and the lookup it started
// keeps running for the caller coalesced onto it (it runs on WithoutCancel).
// ClientCache's own coalescing is bypassed here, so this pins the App.
func TestApp_LookupInstallIDCallerCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 7})
		release := make(chan struct{})
		fake.block["acme"] = release
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")

		ctx, cancel := context.WithCancel(t.Context())
		starter := goLookup(ctx, app, "acme")
		synctest.Wait()
		joiner := goLookup(t.Context(), app, "acme")
		synctest.Wait()

		cancel()
		synctest.Wait()
		select {
		case r := <-starter:
			if !errors.Is(r.err, context.Canceled) {
				t.Fatalf("cancelled lookup: got = %v, want = %v", r.err, context.Canceled)
			}
		default:
			t.Fatal("cancelled lookup is still waiting on the shared lookup")
		}

		close(release)
		synctest.Wait()
		r := <-joiner
		if r.err != nil || r.id != 7 {
			t.Fatalf("joined lookup: got = (%d, %v), want = (7, nil)", r.id, r.err)
		}
		if got := fake.lookups("acme"); got != 1 {
			t.Errorf("lookups: got = %d, want = 1", got)
		}
	})
}

// An owner the App is installed on as a user resolves through the user
// endpoint after the organization endpoint's 404.
func TestApp_LookupInstallIDUserFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{})
		fake.users["octocat"] = 11
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")

		id, err := app.LookupInstallID(t.Context(), "octocat")
		if err != nil || id != 11 {
			t.Fatalf("LookupInstallID: got = (%d, %v), want = (11, nil)", id, err)
		}
		if got := fake.lookups("octocat"); got != 2 {
			t.Errorf("lookups: got = %d, want = 2 (organization, then user)", got)
		}
	})
}

// A lookup GitHub never answers is bounded by the lookup timeout rather than
// hanging every caller coalesced onto it, and a timeout is not remembered.
func TestApp_LookupInstallIDTimeoutNotCached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 1})
		release := make(chan struct{})
		fake.block["acme"] = release
		const timeout = 10 * time.Second
		app := newTestApp(t, handlerTransport{fake}, "http://github.test", WithInstallLookupTimeout(timeout))

		start := time.Now()
		_, err := app.LookupInstallID(t.Context(), "acme")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("LookupInstallID: got = %v, want = %v", err, context.DeadlineExceeded)
		}
		if got := time.Since(start); got != timeout {
			t.Errorf("gave up after: got = %v, want = %v", got, timeout)
		}

		close(release)
		id, err := app.LookupInstallID(t.Context(), "acme")
		if err != nil || id != 1 {
			t.Fatalf("LookupInstallID after the timeout: got = (%d, %v), want = (1, nil)", id, err)
		}
		if got := fake.lookups("acme"); got != 2 {
			t.Errorf("lookups: got = %d, want = 2 (the timeout was not remembered)", got)
		}
	})
}

// "Not installed" is ErrNoInstallation, answered from memory for the
// negative TTL, then asked again, so an install that lands in the meantime
// is picked up.
func TestApp_LookupInstallIDNegativeTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"other": 1})
		const ttl = 45 * time.Second
		app := newTestApp(t, handlerTransport{fake}, "http://github.test", WithInstallLookupNegativeTTL(ttl))
		ctx := t.Context()

		if _, err := app.LookupInstallID(ctx, "acme"); !errors.Is(err, ErrNoInstallation) {
			t.Fatalf("LookupInstallID before install: got = %v, want = %v", err, ErrNoInstallation)
		}
		fake.setInstall("acme", 7)

		time.Sleep(ttl - time.Second)
		if _, err := app.LookupInstallID(ctx, "acme"); !errors.Is(err, ErrNoInstallation) {
			t.Fatalf("LookupInstallID inside the TTL: got = %v, want the remembered %v", err, ErrNoInstallation)
		}
		if got := fake.lookups("acme"); got != 2 {
			t.Fatalf("lookups inside the TTL: got = %d, want = 2 (organization and user, once)", got)
		}

		time.Sleep(2 * time.Second)
		id, err := app.LookupInstallID(ctx, "acme")
		if err != nil || id != 7 {
			t.Fatalf("LookupInstallID after the TTL: got = (%d, %v), want = (7, nil)", id, err)
		}

		// A hit is cached with no expiry.
		time.Sleep(time.Hour)
		if _, err := app.LookupInstallID(ctx, "acme"); err != nil {
			t.Fatalf("LookupInstallID cached: %v", err)
		}
		if got := fake.lookups("acme"); got != 3 {
			t.Errorf("lookups for a cached hit: got = %d, want = 3", got)
		}
	})
}

// Failures that say nothing about the installation are not remembered and
// are not ErrNoInstallation.
func TestApp_LookupInstallIDFailuresNotCached(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := newFakeGitHub(map[string]int64{"acme": 1})
				fake.lookupStatus = status
				app := newTestApp(t, handlerTransport{fake}, "http://github.test")

				_, err := app.LookupInstallID(t.Context(), "acme")
				if err == nil || errors.Is(err, ErrNoInstallation) {
					t.Fatalf("LookupInstallID: got = %v, want a non-ErrNoInstallation error", err)
				}
				fake.mu.Lock()
				fake.lookupStatus = 0
				fake.mu.Unlock()
				if id, err := app.LookupInstallID(t.Context(), "acme"); err != nil || id != 1 {
					t.Fatalf("LookupInstallID after recovery: got = (%d, %v), want = (1, nil)", id, err)
				}
			})
		})
	}
}

// A late forget for an ID the cache has already replaced leaves the newer
// ID alone.
func TestApp_StaleForgetKeepsNewerID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 2})
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")
		if _, err := app.LookupInstallID(t.Context(), "acme"); err != nil {
			t.Fatalf("LookupInstallID: %v", err)
		}

		app.forgetInstallID("acme", 1)
		if id, err := app.LookupInstallID(t.Context(), "acme"); err != nil || id != 2 {
			t.Fatalf("LookupInstallID: got = (%d, %v), want = (2, nil)", id, err)
		}
		if got := fake.lookups("acme"); got != 1 {
			t.Errorf("lookups: got = %d, want = 1 (the stale forget dropped nothing)", got)
		}

		app.forgetInstallID("acme", 2)
		if _, err := app.LookupInstallID(t.Context(), "acme"); err != nil {
			t.Fatalf("LookupInstallID: %v", err)
		}
		if got := fake.lookups("acme"); got != 2 {
			t.Errorf("lookups after forgetting the current ID: got = %d, want = 2", got)
		}
	})
}

// After an uninstall and reinstall, an hour-long token keeps a client
// failing with 401 until something mints. The 401 expires the token, the
// re-mint answers 404 and evicts, and the next Get recovers. Only a 404 on
// the mint evicts: a 401 there is the App's JWT refused, which says nothing
// about this installation, and a 5xx says nothing at all. A reinstall
// seconds after the token was minted is recovered from just as promptly.
func TestClientCache_RecoversAfterReinstall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mintStatus int
		wantEvict  bool
		after      time.Duration // token age at the reinstall
	}{
		{name: "mint 404 evicts", mintStatus: http.StatusNotFound, wantEvict: true, after: 30 * time.Minute},
		{name: "mint 404 evicts a fresh token", mintStatus: http.StatusNotFound, wantEvict: true, after: 5 * time.Second},
		{name: "mint 401 keeps cache", mintStatus: http.StatusUnauthorized, after: 30 * time.Minute},
		{name: "mint 500 keeps cache", mintStatus: http.StatusInternalServerError, after: 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := newFakeGitHub(map[string]int64{"acme": 1, "other": 9})
				fake.mintStatus = tc.mintStatus
				ctx, cc, app := newInProcessAppCache(t, fake)

				before, err := cc.Get(ctx, "acme", "repo")
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				otherBefore, err := cc.Get(ctx, "other", "repo")
				if err != nil {
					t.Fatalf("Get(other): %v", err)
				}
				if _, _, err := before.Repositories.Get(ctx, "acme", "repo"); err != nil {
					t.Fatalf("call before reinstall: %v", err)
				}

				// Well inside the token's hour, so nothing below expires by time.
				time.Sleep(tc.after)
				fake.setInstall("acme", 2) // installation 1 is gone

				_, resp, err := before.Repositories.Get(ctx, "acme", "repo")
				if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("first call after reinstall: got = %v, want 401", err)
				}
				if got := fake.mints(1); got != 1 {
					t.Fatalf("mints after the 401: got = %d, want = 1 (the 401 alone mints nothing)", got)
				}
				if _, _, err := before.Repositories.Get(ctx, "acme", "repo"); err == nil {
					t.Fatal("second call after reinstall: got nil error, want the refused mint")
				}
				if got := fake.mints(1); got != 2 {
					t.Fatalf("mints after the second call: got = %d, want = 2 (the 401 expired the token)", got)
				}

				after, err := cc.Get(ctx, "acme", "repo")
				if err != nil {
					t.Fatalf("Get after reinstall: %v", err)
				}
				if evicted := after != before; evicted != tc.wantEvict {
					t.Fatalf("client replaced: got = %t, want = %t", evicted, tc.wantEvict)
				}
				if otherAfter, err := cc.Get(ctx, "other", "repo"); err != nil || otherAfter != otherBefore {
					t.Errorf("another org's client was dropped (err=%v)", err)
				}
				id, err := app.LookupInstallID(ctx, "acme")
				if err != nil {
					t.Fatalf("LookupInstallID: %v", err)
				}
				wantID := int64(1)
				if tc.wantEvict {
					wantID = 2
				}
				if id != wantID {
					t.Errorf("installation ID: got = %d, want = %d", id, wantID)
				}
				if !tc.wantEvict {
					return
				}
				if _, _, err := after.Repositories.Get(ctx, "acme", "repo"); err != nil {
					t.Fatalf("call on the reinstalled installation: %v", err)
				}
			})
		})
	}
}

// A caller that only takes raw sources from TokenSourceFor (a git clone,
// syftscan) recovers too: once its source's mint answers 404, the org is
// evicted from the cache, and the next TokenSourceFor builds a fresh source
// on the new installation.
func TestClientCache_TokenSourceForRecoversAfterReinstall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 1})
		ctx, cc, _ := newInProcessAppCache(t, fake)

		stale, err := cc.TokenSourceFor(ctx, "acme", "repo")
		if err != nil {
			t.Fatalf("TokenSourceFor: %v", err)
		}
		if _, err := stale.Token(); err != nil {
			t.Fatalf("Token: %v", err)
		}

		fake.setInstall("acme", 2)
		time.Sleep(fakeTokenTTL) // the raw source re-mints when its token expires
		if _, err := stale.Token(); err == nil {
			t.Fatal("Token on the removed installation: got nil error")
		}

		fresh, err := cc.TokenSourceFor(ctx, "acme", "repo")
		if err != nil {
			t.Fatalf("TokenSourceFor after reinstall: %v", err)
		}
		if fresh == stale {
			t.Fatal("TokenSourceFor returned the stale source after its installation was gone")
		}
		tok, err := fresh.Token()
		if err != nil {
			t.Fatalf("Token on the reinstalled installation: %v", err)
		}
		fake.mu.Lock()
		gotID := fake.tokens[tok.AccessToken]
		fake.mu.Unlock()
		if gotID != 2 {
			t.Errorf("token minted for installation: got = %d, want = 2", gotID)
		}
		if got := fake.mints(2); got != 1 {
			t.Errorf("mints on the reinstalled installation: got = %d, want = 1", got)
		}
	})
}

// A failure on an old entry evicts only entries at or before it, and an org
// is matched by its whole login.
func TestClientCache_EvictOrgCompareAndPrefix(t *testing.T) {
	cc := NewClientCache(mockTokenSourceFunc)
	ctx := t.Context()
	entry := func(org, repo string) *cacheEntry {
		t.Helper()
		if _, err := cc.Get(ctx, org, repo); err != nil {
			t.Fatalf("Get(%s/%s): %v", org, repo, err)
		}
		e, ok := cc.entries.get(cc.getKey(org, repo))
		if !ok {
			t.Fatalf("no entry for %s/%s", org, repo)
		}
		return e
	}

	old := entry("acme", "repo")
	cc.evictOrg("acme", old.seq, 0)
	if _, ok := cc.entries.get("acme/repo"); ok {
		t.Fatal("evictOrg left the entry it was stamped for")
	}

	newer := entry("acme", "repo")
	cc.evictOrg("acme", old.seq, 0) // a late failure from the old client
	if e, ok := cc.entries.get("acme/repo"); !ok || e != newer {
		t.Fatal("a late eviction for an old entry removed a newer one")
	}

	entry("acme-corp", "repo")
	cc.evictOrg("acme", math.MaxUint64, 0)
	if _, ok := cc.entries.get("acme/repo"); ok {
		t.Error("evictOrg(acme) left acme/repo")
	}
	if _, ok := cc.entries.get("acme-corp/repo"); !ok {
		t.Error("evictOrg(acme) removed acme-corp/repo")
	}
}

// When the failing entry and a cached entry both say which installation they
// mint for, eviction follows the installation, not creation order: a late 404
// on a source that resolved the old installation but was stamped after a
// sibling bound to the replacement leaves that sibling alone.
func TestClientCache_EvictOrgByInstallation(t *testing.T) {
	cc := NewClientCache(mockTokenSourceFunc)
	replacement := &cacheEntry{seq: 1, installID: 2}
	staleSibling := &cacheEntry{seq: 3, installID: 1}
	unknown := &cacheEntry{seq: 2}
	cc.entries.add("acme/new", replacement)
	cc.entries.add("acme/old", staleSibling)
	cc.entries.add("acme/custom", unknown)

	// The stale source, stamped seq 4 and bound to installation 1, gets a 404.
	cc.evictOrg("acme", 4, 1)
	if e, ok := cc.entries.get("acme/new"); !ok || e != replacement {
		t.Error("a 404 for installation 1 evicted an entry bound to installation 2")
	}
	if _, ok := cc.entries.get("acme/old"); ok {
		t.Error("a 404 for installation 1 left another entry bound to it")
	}
	if _, ok := cc.entries.get("acme/custom"); ok {
		t.Error("an older entry of unknown installation survived the 404")
	}
}

// WithAppOptions reaches the App AppMain builds.
func TestWithAppOptionsReachesAppMainApp(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(testAppKey()),
	}), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	app, err := newAppFor(t.Context(), 210473, "file://"+keyPath, []MainOption{
		WithIdentity("test"),
		WithAppOptions(WithInstallLookupTimeout(7*time.Second), WithInstallLookupNegativeTTL(3*time.Minute)),
	})
	if err != nil {
		t.Fatalf("newAppFor: %v", err)
	}
	if app.lookupTimeout != 7*time.Second || app.negativeTTL != 3*time.Minute {
		t.Errorf("App options: got = (%v, %v), want = (7s, 3m0s)", app.lookupTimeout, app.negativeTTL)
	}
}

// alwaysUnauthorized serves fake, except that /unauthorized answers 401 to
// every request, whatever token it carries. When gate is set, each request
// to it reports on arrived and waits for gate to close.
type alwaysUnauthorized struct {
	fake    *fakeGitHub
	arrived chan struct{}
	gate    chan struct{}
}

func (h *alwaysUnauthorized) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/unauthorized" {
		h.fake.ServeHTTP(w, r)
		return
	}
	if h.gate != nil {
		h.arrived <- struct{}{}
		<-h.gate
	}
	http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
}

func callUnauthorized(ctx context.Context, t *testing.T, client *github.Client) {
	t.Helper()
	req, err := client.NewRequest(ctx, http.MethodGet, "unauthorized", nil)
	if err != nil {
		t.Errorf("NewRequest: %v", err)
		return
	}
	if _, err := client.Do(req, nil); err == nil {
		t.Error("call to /unauthorized: got nil error")
	}
}

// newUnauthorizedCache is newInProcessAppCache with /unauthorized served by h.
func newUnauthorizedCache(t *testing.T, h *alwaysUnauthorized) (context.Context, *ClientCache) {
	t.Helper()
	tr := handlerTransport{h}
	app := newTestApp(t, tr, "http://github.test")
	cc := newClientCacheFor(mainOptions{
		tsff:          func(string) TokenSourceFunc { return app.TokenSourceFunc() },
		installIDFunc: app.LookupInstallID,
	}, "test")
	return context.WithValue(t.Context(), oauth2.HTTPClient, &http.Client{Transport: tr}), cc
}

// A 401 that has nothing to do with the token does not mint once per
// request: after one forced re-mint, no other is forced for
// minForcedRemintInterval, so 50 calls, sequential or concurrent, cost at
// most one forced re-mint.
func TestClientCache_Unrelated401sDoNotMintPerRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		concurrent bool
		age        time.Duration
	}{
		{name: "sequential fresh token", age: 0},
		{name: "sequential old token", age: 2 * time.Minute},
		{name: "concurrent fresh token", concurrent: true, age: 0},
		{name: "concurrent old token", concurrent: true, age: 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := newFakeGitHub(map[string]int64{"acme": 1})
				ctx, cc := newUnauthorizedCache(t, &alwaysUnauthorized{fake: fake})
				client, err := cc.Get(ctx, "acme", "repo")
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if _, _, err := client.Repositories.Get(ctx, "acme", "repo"); err != nil {
					t.Fatalf("call: %v", err)
				}
				time.Sleep(tc.age)

				if tc.concurrent {
					var wg sync.WaitGroup
					for range 20 {
						wg.Go(func() { callUnauthorized(ctx, t, client) })
					}
					wg.Wait()
				} else {
					for range 50 {
						callUnauthorized(ctx, t, client)
					}
				}
				// One more call, so a token a 401 expired is re-minted here.
				callUnauthorized(ctx, t, client)
				if got := fake.mints(1); got > 2 {
					t.Errorf("mints: got = %d, want <= 2", got)
				}
			})
		})
	}
}

// A burst of 401s that all carried the same old token forces exactly one
// re-mint: the first expires it, and the rest find it already replaced.
func TestClientCache_401BurstForcesOneMint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 1})
		h := &alwaysUnauthorized{fake: fake, arrived: make(chan struct{}), gate: make(chan struct{})}
		ctx, cc := newUnauthorizedCache(t, h)
		client, err := cc.Get(ctx, "acme", "repo")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if _, _, err := client.Repositories.Get(ctx, "acme", "repo"); err != nil {
			t.Fatalf("call: %v", err)
		}
		time.Sleep(2 * time.Minute)

		const burst = 20
		var wg sync.WaitGroup
		for range burst {
			wg.Go(func() { callUnauthorized(ctx, t, client) })
		}
		for range burst {
			<-h.arrived // every request is in flight with the old token
		}
		close(h.gate)
		wg.Wait()

		if _, _, err := client.Repositories.Get(ctx, "acme", "repo"); err != nil {
			t.Fatalf("call after the burst: %v", err)
		}
		if got := fake.mints(1); got != 2 {
			t.Errorf("mints: got = %d, want = 2 (the first token, then one forced re-mint)", got)
		}
	})
}

// A late expiry for a token that has already been replaced leaves the
// replacement alone, in the App source and in the reuse layer above it.
func TestExpireTokenStaleIsNoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 1})
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")

		for _, layer := range []struct {
			name string
			ts   func() interface {
				oauth2.TokenSource
				tokenExpirer
			}
		}{
			{name: "appTokenSource", ts: func() interface {
				oauth2.TokenSource
				tokenExpirer
			} {
				return app.installationTokenSource(t.Context(), 1, "")
			}},
			{name: "expirableTokenSource", ts: func() interface {
				oauth2.TokenSource
				tokenExpirer
			} {
				return &expirableTokenSource{src: app.installationTokenSource(t.Context(), 1, "")}
			}},
		} {
			ts := layer.ts()
			start := fake.mints(1)
			first, err := ts.Token()
			if err != nil {
				t.Fatalf("%s: Token: %v", layer.name, err)
			}
			time.Sleep(2 * time.Minute)
			ts.expireToken(first.AccessToken)
			second, err := ts.Token()
			if err != nil || second.AccessToken == first.AccessToken {
				t.Fatalf("%s: Token after expiry: got = (%v, %v), want a new token", layer.name, second, err)
			}
			time.Sleep(2 * time.Minute)
			ts.expireToken(first.AccessToken) // late, for the replaced token
			third, err := ts.Token()
			if err != nil || third.AccessToken != second.AccessToken {
				t.Errorf("%s: a stale expiry replaced the current token", layer.name)
			}
			if got := fake.mints(1) - start; got != 2 {
				t.Errorf("%s: mints: got = %d, want = 2", layer.name, got)
			}
		}
	})
}

// evictOnGone covers any TokenSourceFunc whose errors carry ghinstallation's
// HTTPError, not only an App's: a 404 evicts the org, anything else keeps it.
func TestClientCache_EvictOnGoneCustomTokenSource(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantEvict bool
	}{
		{name: "404 evicts", status: http.StatusNotFound, wantEvict: true},
		{name: "401 keeps", status: http.StatusUnauthorized},
		{name: "500 keeps", status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := NewClientCache(func(context.Context, string, string) (oauth2.TokenSource, error) {
				return failingTokenSource{err: fmt.Errorf("minting: %w", &ghinstallation.HTTPError{
					Message:  http.StatusText(tc.status),
					Response: &http.Response{StatusCode: tc.status},
				})}, nil
			})
			ctx := t.Context()
			before, err := cc.Get(ctx, "acme", "repo")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if _, _, err := before.Repositories.Get(ctx, "acme", "repo"); err == nil {
				t.Fatal("call: got nil error, want the mint failure")
			}
			after, err := cc.Get(ctx, "acme", "repo")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if evicted := after != before; evicted != tc.wantEvict {
				t.Errorf("client replaced: got = %t, want = %t", evicted, tc.wantEvict)
			}
		})
	}
}

type failingTokenSource struct{ err error }

func (f failingTokenSource) Token() (*oauth2.Token, error) { return nil, f.err }

// Owners that are not GitHub logins are refused before any request.
func TestApp_LookupInstallIDRejectsInvalidOwners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 1})
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")
		for _, owner := range []string{"victim?", "x/y", "a#b", "", "-acme", "acme..", "a%2Fb", strings.Repeat("a", 40)} {
			if _, err := app.LookupInstallID(t.Context(), owner); !errors.Is(err, ErrNoInstallation) {
				t.Errorf("LookupInstallID(%q): got = %v, want %v", owner, err, ErrNoInstallation)
			}
		}
		if got := fake.totalLookups(); got != 0 {
			t.Errorf("requests for invalid owners: got = %d, want = 0", got)
		}
		for _, owner := range []string{"acme", "a", "my-org", "handle_shortcode", strings.Repeat("a", 39)} {
			if !validOwner(owner) {
				t.Errorf("validOwner(%q): got = false, want = true", owner)
			}
		}
	})
}

// Logins are case-insensitive: every spelling shares one cache entry, and a
// forget in any spelling drops it.
func TestApp_LookupInstallIDCaseInsensitive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 5})
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")
		for _, owner := range []string{"acme", "Acme", "ACME"} {
			if id, err := app.LookupInstallID(t.Context(), owner); err != nil || id != 5 {
				t.Fatalf("LookupInstallID(%q): got = (%d, %v), want = (5, nil)", owner, id, err)
			}
		}
		if got := fake.lookups("acme"); got != 1 {
			t.Errorf("lookups: got = %d, want = 1", got)
		}
		app.forgetInstallID("AcMe", 5)
		if _, err := app.LookupInstallID(t.Context(), "acme"); err != nil {
			t.Fatalf("LookupInstallID: %v", err)
		}
		if got := fake.lookups("acme"); got != 2 {
			t.Errorf("lookups after a forget in another spelling: got = %d, want = 2", got)
		}
	})
}

// An answer for a different account than the one asked about is treated as
// not installed.
func TestApp_LookupInstallIDMismatchedLogin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 5})
		fake.login["acme"] = "someone-else"
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")
		if _, err := app.LookupInstallID(t.Context(), "acme"); !errors.Is(err, ErrNoInstallation) {
			t.Fatalf("LookupInstallID: got = %v, want %v", err, ErrNoInstallation)
		}
	})
}

// The ClientCache folds the org the same way.
func TestClientCache_OrgCaseInsensitive(t *testing.T) {
	cc := NewClientCache(mockTokenSourceFunc)
	ctx := t.Context()
	a, err := cc.Get(ctx, "Acme", "repo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, err := cc.Get(ctx, "acme", "repo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a != b {
		t.Error("Acme/repo and acme/repo got different clients")
	}
	cc.evictOrg("ACME", math.MaxUint64, 0)
	if got := cc.entries.len(); got != 0 {
		t.Errorf("entries after evictOrg(ACME): got = %d, want = 0", got)
	}
}

// The clients and token sources are bounded, evicting the least recently
// used key.
func TestClientCache_LRUBound(t *testing.T) {
	var created atomic.Int32
	cc := NewClientCache(func(_ context.Context, org, repo string) (oauth2.TokenSource, error) {
		created.Add(1)
		return &mockTokenSource{token: org + "/" + repo}, nil
	}, WithClientCacheSize(2))
	ctx := t.Context()

	get := func(repo string) *github.Client {
		t.Helper()
		c, err := cc.Get(ctx, "org", repo)
		if err != nil {
			t.Fatalf("Get(%s): %v", repo, err)
		}
		return c
	}

	a := get("a")
	get("b")
	get("a") // a is now more recent than b
	get("c") // evicts b

	if got := cc.entries.len(); got != 2 {
		t.Errorf("entries held: got = %d, want = 2", got)
	}
	if again := get("a"); again != a {
		t.Error("recently used client a was evicted")
	}
	if got := created.Load(); got != 3 {
		t.Fatalf("token sources created before re-getting b: got = %d, want = 3", got)
	}
	get("b")
	if got := created.Load(); got != 4 {
		t.Errorf("token sources created after re-getting evicted b: got = %d, want = 4", got)
	}
}

func TestWithClientCacheSizeNonPositiveKeepsDefault(t *testing.T) {
	for _, n := range []int{0, -1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			cc := NewClientCache(mockTokenSourceFunc, WithClientCacheSize(n))
			for i := range DefaultClientCacheSize + 1 {
				if _, err := cc.Get(t.Context(), "org", fmt.Sprint(i)); err != nil {
					t.Fatalf("Get: %v", err)
				}
			}
			if got := cc.entries.len(); got != DefaultClientCacheSize {
				t.Errorf("clients held: got = %d, want = %d", got, DefaultClientCacheSize)
			}
		})
	}
}

// The owner filter decides before the client cache is touched: a rejected
// owner mints nothing, and a filter error is retried rather than dropped.
func TestReconciler_OwnerFilter(t *testing.T) {
	filterErr := errors.New("entitlements unavailable")
	for _, tc := range []struct {
		name          string
		allow         bool
		err           error
		wantErr       error
		wantReconcile bool
	}{
		{name: "allowed", allow: true, wantReconcile: true},
		{name: "rejected is dropped", allow: false},
		{name: "error is retried", err: filterErr, wantErr: filterErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted, reconciled atomic.Int32
			cc := NewClientCache(func(_ context.Context, org, repo string) (oauth2.TokenSource, error) {
				minted.Add(1)
				return &mockTokenSource{token: org + "/" + repo}, nil
			})
			var gotOwner string
			r := NewReconciler(cc,
				WithReconciler(func(context.Context, *Resource, *github.Client) error {
					reconciled.Add(1)
					return nil
				}),
				WithReconcilerOwnerFilter(func(_ context.Context, owner string) (bool, error) {
					gotOwner = owner
					return tc.allow, tc.err
				}),
			)

			// A mixed-case spelling reaches the filter folded, as it reaches
			// the cache and the installation lookup.
			err := r.Reconcile(t.Context(), "https://github.com/AcMe/repo/pull/1")
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("Reconcile: got = %v, want = %v", err, tc.wantErr)
			}
			if gotOwner != "acme" {
				t.Errorf("filtered owner: got = %q, want = %q", gotOwner, "acme")
			}
			if got, want := reconciled.Load() == 1, tc.wantReconcile; got != want {
				t.Errorf("reconciled: got = %t, want = %t", got, want)
			}
			if got, want := minted.Load() == 1, tc.wantReconcile; got != want {
				t.Errorf("token source created: got = %t, want = %t", got, want)
			}
		})
	}
}

// WithOwnerFilter reaches the reconciler Main builds.
func TestWithOwnerFilterWiring(t *testing.T) {
	var mo mainOptions
	WithOwnerFilter(func(context.Context, string) (bool, error) { return false, nil })(&mo)
	r := NewReconciler(NewClientCache(mockTokenSourceFunc), mo.reconcilerOpts...)
	if r.ownerFilter == nil {
		t.Fatal("WithOwnerFilter did not set the reconciler's owner filter")
	}
}

// A panicking token source function fails the Get instead of crashing the
// process from singleflight's goroutine.
func TestClientCache_TokenSourceFuncPanicIsAnError(t *testing.T) {
	cc := NewClientCache(func(context.Context, string, string) (oauth2.TokenSource, error) {
		panic("boom")
	})
	if _, err := cc.Get(t.Context(), "org", "repo"); err == nil {
		t.Fatal("Get: got nil error, want the recovered panic")
	}
}

// countingTokenSource hands out one fixed token and counts fetches.
type countingTokenSource struct{ fetches atomic.Int32 }

func (c *countingTokenSource) Token() (*oauth2.Token, error) {
	c.fetches.Add(1)
	return &oauth2.Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)}, nil
}

// The reuse layer drops a token on the first expiry however young it is,
// then refuses another forced drop for minForcedRemintInterval, on its own,
// whatever the source below does.
func TestExpirableTokenSourceRemintInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &countingTokenSource{}
		ts := &expirableTokenSource{src: src}
		fetch := func() {
			t.Helper()
			if _, err := ts.Token(); err != nil {
				t.Fatalf("Token: %v", err)
			}
		}
		fetch()
		ts.expireToken("some-other-token")
		fetch()
		if got := src.fetches.Load(); got != 1 {
			t.Fatalf("fetches after expiring a token that is not the cached one: got = %d, want = 1", got)
		}
		ts.expireToken("tok")
		fetch()
		if got := src.fetches.Load(); got != 2 {
			t.Fatalf("fetches after the first expiry of a fresh token: got = %d, want = 2", got)
		}
		ts.expireToken("tok")
		fetch()
		if got := src.fetches.Load(); got != 2 {
			t.Fatalf("fetches after a second expiry within the interval: got = %d, want = 2", got)
		}
		time.Sleep(minForcedRemintInterval)
		ts.expireToken("tok")
		fetch()
		if got := src.fetches.Load(); got != 3 {
			t.Errorf("fetches after an expiry once the interval passed: got = %d, want = 3", got)
		}
	})
}

// The App source applies the same interval on its own: the first expiry
// re-mints a fresh installation token, and no later one within the interval
// does, however many 401s ask.
func TestAppTokenSourceRemintInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeGitHub(map[string]int64{"acme": 1})
		app := newTestApp(t, handlerTransport{fake}, "http://github.test")
		ts := app.installationTokenSource(t.Context(), 1, "")
		tok, err := ts.Token()
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		for range 10 {
			ts.expireToken(tok.AccessToken)
			if tok, err = ts.Token(); err != nil {
				t.Fatalf("Token: %v", err)
			}
		}
		if got := fake.mints(1); got != 2 {
			t.Errorf("mints: got = %d, want = 2", got)
		}
	})
}
