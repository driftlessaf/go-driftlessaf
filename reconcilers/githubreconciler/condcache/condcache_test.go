/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package condcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// origin is a fake GitHub that answers a single resource conditionally, the
// way the real one does: an ETag on the body, 304 when the caller's
// If-None-Match still matches, and rate-limit headers on every response
// whether or not a body comes with it.
type origin struct {
	mu sync.Mutex
	// body and etag are the current representation; change them to move it.
	body, etag string
	// remaining is decremented on every response that is NOT a 304, mirroring
	// the one behaviour this package exists to exploit.
	remaining int
	// requests counts every request, conditional or not; notModified counts
	// those answered 304.
	requests, notModified int
	// status, when non-zero, is returned instead of the normal answer.
	status int
	// omitETag serves the body with no validator.
	omitETag bool
}

// handlerFor serves o. A function rather than a method on *origin: the
// receiver would only ever be captured by the closure below, which reads as
// an unused receiver.
func handlerFor(o *origin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.requests++

		conditional := r.Header.Get("If-None-Match") == o.etag && o.etag != ""
		if !conditional {
			o.remaining--
		} else {
			o.notModified++
		}

		w.Header().Set("X-Ratelimit-Limit", "15000")
		w.Header().Set("X-Ratelimit-Remaining", strconv.Itoa(o.remaining))
		w.Header().Set("X-Ratelimit-Resource", "core")
		w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)

		if o.status != 0 {
			w.WriteHeader(o.status)
			return
		}
		if conditional {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if !o.omitETag {
			w.Header().Set("ETag", o.etag)
		}
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test server
		w.Write([]byte(o.body))
	})
}

// result is one completed exchange. The body is read and the response closed
// inside get, so what comes back is a value rather than a live response whose
// body a caller could neither read nor is expected to close.
type result struct {
	status int
	header http.Header
	body   string
}

// get issues one GET through the transport and reads it to completion.
func get(t *testing.T, rt http.RoundTripper, url string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func newOrigin(t *testing.T, o *origin) (*Transport, string) {
	t.Helper()
	srv := httptest.NewServer(handlerFor(o))
	t.Cleanup(srv.Close)
	return New(srv.Client().Transport), srv.URL + "/resource"
}

// TestRevalidatedReadCostsNoQuota is the whole point: a repeated read of an
// unchanged resource still reaches GitHub, but as a 304, which does not draw
// on the rate limit. The origin's remaining counter is the assertion — it is
// decremented only on a non-304, exactly as GitHub's is.
func TestRevalidatedReadCostsNoQuota(t *testing.T) {
	o := &origin{body: `{"number":7,"state":"open"}`, etag: `W/"abc123"`, remaining: 15000}
	rt, url := newOrigin(t, o)

	for i := range 5 {
		got := get(t, rt, url)
		if got.status != http.StatusOK {
			t.Fatalf("read %d: status = %d, want 200 (a 304 must never reach the caller)", i, got.status)
		}
		if got.body != o.body {
			t.Errorf("read %d: body = %q, want = %q", i, got.body, o.body)
		}
	}

	if got, want := o.requests, 5; got != want {
		t.Errorf("origin requests: got = %d, want = %d (every read still revalidates)", got, want)
	}
	if got, want := o.notModified, 4; got != want {
		t.Errorf("304 responses: got = %d, want = %d", got, want)
	}
	if got, want := o.remaining, 14999; got != want {
		t.Errorf("rate limit remaining: got = %d, want = %d (only the first read should cost quota)", got, want)
	}
}

// TestHitCarriesFreshRateLimitHeaders pins the subtlety that makes the replay
// safe to instrument. The body is remembered, but the budget is not: a hit
// must report the quota as of this revalidation, or every metric and
// backoff decision downstream reads an hours-old number.
func TestHitCarriesFreshRateLimitHeaders(t *testing.T) {
	o := &origin{body: "{}", etag: `W/"v1"`, remaining: 900}
	rt, url := newOrigin(t, o)

	first := get(t, rt, url)
	if got := first.header.Get("X-Ratelimit-Remaining"); got != "899" {
		t.Fatalf("first read remaining: got = %q, want = \"899\"", got)
	}

	// Spend quota elsewhere, then read again: the body is unchanged, the
	// budget is not.
	o.mu.Lock()
	o.remaining = 42
	o.mu.Unlock()

	second := get(t, rt, url)
	if got, want := second.header.Get("X-Ratelimit-Remaining"), "42"; got != want {
		t.Errorf("replayed remaining: got = %q, want = %q (the 304's value, not the remembered one)", got, want)
	}
	if second.body != o.body {
		t.Errorf("replayed body: got = %q, want = %q", second.body, o.body)
	}
	// Headers that describe the representation still come from the cache.
	if got := second.header.Get("ETag"); got != `W/"v1"` {
		t.Errorf("replayed ETag: got = %q, want = %q", got, `W/"v1"`)
	}
	if got := second.header.Get("Link"); !strings.Contains(got, "page=2") {
		t.Errorf("replayed Link: got = %q, want the remembered pagination link", got)
	}
}

// TestChangedRepresentationIsServedAndRemembered pins that a moved resource
// is not masked by the cache, and that the new body replaces the old one.
func TestChangedRepresentationIsServedAndRemembered(t *testing.T) {
	o := &origin{body: `{"state":"open"}`, etag: `W/"v1"`, remaining: 15000}
	rt, url := newOrigin(t, o)

	if got := get(t, rt, url); got.body != `{"state":"open"}` {
		t.Fatalf("first read: body = %q", got.body)
	}

	o.mu.Lock()
	o.body, o.etag = `{"state":"closed"}`, `W/"v2"`
	o.mu.Unlock()

	if got := get(t, rt, url); got.body != `{"state":"closed"}` {
		t.Errorf("after the resource moved: body = %q, want the new representation", got.body)
	}
	// The new representation must now be the one revalidated against.
	if got := get(t, rt, url); got.body != `{"state":"closed"}` {
		t.Errorf("third read: body = %q, want the new representation", got.body)
	}
	if got, want := o.notModified, 1; got != want {
		t.Errorf("304 responses: got = %d, want = %d (only the third read matches)", got, want)
	}
}

// TestPassThrough covers everything the Transport must not touch.
func TestPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		prepare func(*origin)
		opts    []Option
		// wantQuotaSpent is how much the origin's budget drops over two reads;
		// 2 means nothing was cached.
		wantQuotaSpent int
	}{{
		name:           "a response with no ETag is not remembered",
		method:         http.MethodGet,
		prepare:        func(o *origin) { o.omitETag = true },
		wantQuotaSpent: 2,
	}, {
		name:           "a non-200 is not remembered",
		method:         http.MethodGet,
		prepare:        func(o *origin) { o.status = http.StatusInternalServerError },
		wantQuotaSpent: 2,
	}, {
		name:           "caching disabled",
		method:         http.MethodGet,
		opts:           []Option{WithMaxEntries(0)},
		wantQuotaSpent: 2,
	}, {
		name:           "a body over the size cap is not remembered",
		method:         http.MethodGet,
		prepare:        func(o *origin) { o.body = strings.Repeat("x", 2048) },
		opts:           []Option{WithMaxEntryBytes(512)},
		wantQuotaSpent: 2,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			o := &origin{body: "{}", etag: `W/"v1"`, remaining: 15000}
			if tc.prepare != nil {
				tc.prepare(o)
			}
			srv := httptest.NewServer(handlerFor(o))
			t.Cleanup(srv.Close)
			rt := New(srv.Client().Transport, tc.opts...)
			url := srv.URL + "/resource"

			before := o.remaining
			want := o.body
			for range 2 {
				got := get(t, rt, url)
				if tc.prepare == nil || o.status == 0 {
					if got.body != want {
						t.Errorf("body = %q, want = %q", got.body, want)
					}
				}
			}
			if got := before - o.remaining; got != tc.wantQuotaSpent {
				t.Errorf("quota spent over two reads: got = %d, want = %d", got, tc.wantQuotaSpent)
			}
		})
	}
}

// TestNonGETIsUntouched pins that writes never revalidate: a cached
// representation has nothing to say about a POST, and a conditional header on
// one would change what the server does.
func TestNonGETIsUntouched(t *testing.T) {
	var sawConditional bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			sawConditional = true
		}
		w.Header().Set("ETag", `W/"v1"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	rt := New(srv.Client().Transport)

	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/x", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		resp.Body.Close()
	}
	if sawConditional {
		t.Error("a POST carried If-None-Match; writes must pass through untouched")
	}
}

// TestCallerConditionalRequestIsRespected pins that a caller already doing
// conditional requests keeps seeing the 304 it asked for, rather than having
// this package answer on its behalf.
func TestCallerConditionalRequestIsRespected(t *testing.T) {
	o := &origin{body: "{}", etag: `W/"v1"`, remaining: 15000}
	rt, url := newOrigin(t, o)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("If-None-Match", `W/"v1"`)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304 (the caller's own conditional request must survive)", resp.StatusCode)
	}
}

// TestRequestIsNotMutated pins that the conditional header goes on a clone.
// The caller owns its request; a header set on it here would outlive the call
// and turn a later retry into a conditional request it never asked for.
func TestRequestIsNotMutated(t *testing.T) {
	o := &origin{body: "{}", etag: `W/"v1"`, remaining: 15000}
	rt, url := newOrigin(t, o)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("first RoundTrip: %v", err)
	}
	resp.Body.Close()

	resp, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("second RoundTrip: %v", err)
	}
	resp.Body.Close()

	if got := req.Header.Get("If-None-Match"); got != "" {
		t.Errorf("caller's request carries If-None-Match = %q after the call; it must be set on a clone", got)
	}
}

// TestOversizedBodyIsStillDeliveredWhole pins that declining to cache never
// truncates: the cap is a memory bound, not a correctness one.
func TestOversizedBodyIsStillDeliveredWhole(t *testing.T) {
	want := strings.Repeat("y", 5000)
	o := &origin{body: want, etag: `W/"v1"`, remaining: 15000}
	srv := httptest.NewServer(handlerFor(o))
	t.Cleanup(srv.Close)
	rt := New(srv.Client().Transport, WithMaxEntryBytes(64))

	got := get(t, rt, srv.URL+"/resource")
	if got.body != want {
		t.Errorf("body length = %d, want = %d (an uncacheable body must still arrive whole)", len(got.body), len(want))
	}
}

// TestKeyedByAccept pins that two representations of one URL do not collide.
// GitHub serves different media types at the same path — raw blob bytes
// versus the JSON envelope — and serving one where the other was asked for
// would be a decoding failure at best.
func TestKeyedByAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		w.Header().Set("ETag", `W/"`+accept+`"`)
		if r.Header.Get("If-None-Match") == `W/"`+accept+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test server
		w.Write([]byte("body for " + accept))
	}))
	t.Cleanup(srv.Close)
	rt := New(srv.Client().Transport)

	for _, accept := range []string{"application/vnd.github+json", "application/vnd.github.raw"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/same", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Accept", accept)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got, want := string(body), "body for "+accept; got != want {
			t.Errorf("Accept %q: body = %q, want = %q", accept, got, want)
		}
	}
}

// TestConcurrentReadsAreSafe exercises the shared map under -race.
func TestConcurrentReadsAreSafe(t *testing.T) {
	o := &origin{body: "{}", etag: `W/"v1"`, remaining: 1 << 20}
	rt, url := newOrigin(t, o)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 8 {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
				if err != nil {
					return
				}
				resp, err := rt.RoundTrip(req)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}
