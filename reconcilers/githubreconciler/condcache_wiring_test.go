/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

// conditionalOrigin is a fake GitHub that answers one pull request
// conditionally: an ETag on the body, 304 when If-None-Match still matches,
// and a rate-limit counter that only moves on a non-304 — the behaviour the
// whole feature exists to exploit.
type conditionalOrigin struct {
	mu          sync.Mutex
	conditional int
	remaining   int
}

func (o *conditionalOrigin) start(t *testing.T) *httptest.Server {
	t.Helper()
	const etag = `W/"pr-7-v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()

		match := r.Header.Get("If-None-Match") == etag
		if match {
			o.conditional++
		} else {
			o.remaining--
		}

		w.Header().Set("ETag", etag)
		w.Header().Set("X-Ratelimit-Remaining", strconv.Itoa(o.remaining))
		if match {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test server
		w.Write([]byte(`{"number":7,"state":"open"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// clientFor drives the real wiring: MainOptions are applied exactly as Main
// applies them, the ClientCache is built exactly as Main builds it, and the
// client comes back from ClientCache.Get. Nothing here reaches past the
// public option into condcache directly — that is the point.
func clientFor(t *testing.T, srv *httptest.Server, opts ...MainOption) *github.Client {
	t.Helper()

	mo := mainOptions{
		tsff: func(string) TokenSourceFunc {
			return func(context.Context, string, string) (oauth2.TokenSource, error) {
				return &mockTokenSource{token: "test-token"}, nil
			}
		},
	}
	for _, opt := range opts {
		opt(&mo)
	}

	cc := newClientCacheFor(mo, "test-identity")
	client, err := cc.Get(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("ClientCache.Get: %v", err)
	}

	// Point the client at the fake. WithEnterpriseURLs is how the other
	// tests in this tree retarget go-github; it leaves the transport chain
	// the cache built untouched, which is what is under test.
	client, err = github.NewClient(
		github.WithTransport(client.Client().Transport),
		github.WithEnterpriseURLs(srv.URL, srv.URL),
	)
	if err != nil {
		t.Fatalf("retargeting client: %v", err)
	}
	return client
}

// readPR issues one GET through the client, as a reconciler would.
func readPR(t *testing.T, client *github.Client) *github.PullRequest {
	t.Helper()
	pr, resp, err := client.PullRequests.Get(t.Context(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("PullRequests.Get: %v", err)
	}
	resp.Body.Close()
	return pr
}

// TestWithConditionalRequestsIsWired is the end-to-end pin for the wiring
// between the public option and a live client.
//
// condcache is tested thoroughly on its own, but every one of those tests
// constructs the Transport directly. If the chain from
// WithConditionalRequests through mainOptions.wrapTransport and
// newClientCacheFor to ClientCache.Get were broken — a dropped assignment, an
// option that sets the wrong field — the feature would become a silent no-op
// and the entire condcache suite would still pass. This asserts on the fake
// origin's own counters instead: a second read of an unchanged resource must
// arrive as a conditional request and must not cost quota.
func TestWithConditionalRequestsIsWired(t *testing.T) {
	o := &conditionalOrigin{remaining: 5000}
	client := clientFor(t, o.start(t), WithConditionalRequests())

	first := readPR(t, client)
	if got, want := first.GetNumber(), 7; got != want {
		t.Fatalf("first read: pull request number = %d, want = %d", got, want)
	}

	second := readPR(t, client)
	if got, want := second.GetNumber(), 7; got != want {
		t.Errorf("second read: pull request number = %d, want = %d (the replayed body must decode)", got, want)
	}

	if got, want := o.conditional, 1; got != want {
		t.Errorf("conditional requests reaching the origin: got = %d, want = %d; the option is not wired to the client", got, want)
	}
	if got, want := o.remaining, 4999; got != want {
		t.Errorf("rate limit remaining: got = %d, want = %d (the revalidated read must cost no quota)", got, want)
	}
}

// TestWithoutConditionalRequestsEveryReadCostsQuota is the negative control.
// Without it, a wiring bug that made every client conditional — or a fake
// that reports 304s regardless — would look identical to success above.
func TestWithoutConditionalRequestsEveryReadCostsQuota(t *testing.T) {
	o := &conditionalOrigin{remaining: 5000}
	client := clientFor(t, o.start(t))

	readPR(t, client)
	readPR(t, client)

	if got := o.conditional; got != 0 {
		t.Errorf("conditional requests: got = %d, want = 0 (the option is off)", got)
	}
	if got, want := o.remaining, 4998; got != want {
		t.Errorf("rate limit remaining: got = %d, want = %d (both reads cost quota)", got, want)
	}
}

// TestConditionalRequestsAreScopedPerClient pins the safety property the
// package doc rests on: caches must not be shared between clients, because a
// hit serves a remembered body without re-checking authorization. Two clients
// for different repositories must therefore not see each other's entries.
func TestConditionalRequestsAreScopedPerClient(t *testing.T) {
	mo := mainOptions{
		tsff: func(string) TokenSourceFunc {
			return func(context.Context, string, string) (oauth2.TokenSource, error) {
				return &mockTokenSource{token: "test-token"}, nil
			}
		},
	}
	WithConditionalRequests()(&mo)
	cc := newClientCacheFor(mo, "test-identity")

	first, err := cc.Get(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("ClientCache.Get(acme/widgets): %v", err)
	}
	second, err := cc.Get(t.Context(), "globex", "gadgets")
	if err != nil {
		t.Fatalf("ClientCache.Get(globex/gadgets): %v", err)
	}

	if first.Client().Transport == second.Client().Transport {
		t.Error("two repositories share one transport, and so one response cache; a cached body must be reachable only by the credentials that fetched it")
	}
}
