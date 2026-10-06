/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package condcache

import (
	"bytes"
	"cmp"
	"container/list"
	"io"
	"net/http"
	"sync"

	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
)

const (
	// DefaultMaxEntries is how many responses a Transport remembers.
	DefaultMaxEntries = 2048

	// DefaultMaxEntryBytes bounds one remembered body. A response larger than
	// this is returned to the caller untouched and simply not remembered, so
	// the cap costs revalidation opportunities, never correctness.
	//
	// It is sized for the largest responses a reconciler re-reads: a page of
	// 100 check runs carrying their output text, or a page of pulls/{n}/files
	// with patches, runs to hundreds of KiB. Those are the reads whose 304s
	// save the most budget, so a cap below them caches only what is cheap.
	DefaultMaxEntryBytes = 1 << 20

	// DefaultMaxTotalBytes bounds every remembered body together. Without it
	// the real ceiling would be MaxEntries times MaxEntryBytes, which is the
	// number that matters and is far larger than the working set.
	DefaultMaxTotalBytes = 32 << 20
)

// freshHeaders are copied from the 304 onto the replayed response, overriding
// whatever the remembered one carried.
//
// The rate-limit family is the reason this list exists: those headers report
// the budget as of *this* revalidation, and replaying the values from the
// original response would report a budget that may be hours stale — to
// metrics, to alerting, and to any caller that reads Response.Rate to decide
// whether to back off. The rest are per-response facts that belong to the
// exchange that just happened, not to the body being replayed.
var freshHeaders = []string{
	"X-Ratelimit-Limit",
	"X-Ratelimit-Remaining",
	"X-Ratelimit-Reset",
	"X-Ratelimit-Used",
	"X-Ratelimit-Resource",
	"Retry-After",
	"Date",
	"X-Github-Request-Id",
}

// Option configures a Transport.
type Option func(*Transport)

// WithMaxEntries overrides DefaultMaxEntries. A non-positive size disables
// caching, leaving the Transport a pass-through.
func WithMaxEntries(n int) Option { return func(t *Transport) { t.maxEntries = n } }

// WithMaxEntryBytes overrides DefaultMaxEntryBytes.
func WithMaxEntryBytes(n int) Option { return func(t *Transport) { t.maxEntryBytes = n } }

// WithMaxTotalBytes overrides DefaultMaxTotalBytes.
func WithMaxTotalBytes(n int) Option { return func(t *Transport) { t.maxTotalBytes = n } }

// entry is one remembered response.
type entry struct {
	etag   string
	header http.Header
	body   []byte
}

// slot is an entry's place in the recency list, which needs the key to find
// the map entry again when it evicts from the back.
type slot struct {
	key string
	entry
}

// Transport revalidates GETs with If-None-Match and replays the remembered
// body on 304. See the package doc for scope and freshness.
//
// Safe for concurrent use.
type Transport struct {
	base http.RoundTripper

	maxEntries    int
	maxEntryBytes int
	maxTotalBytes int

	mu sync.Mutex
	// entries indexes recency, whose front is the most recently used.
	entries map[string]*list.Element
	recency list.List
	bytes   int
}

var _ http.RoundTripper = (*Transport)(nil)

// New returns a Transport wrapping base. A nil base uses
// http.DefaultTransport.
func New(base http.RoundTripper, opts ...Option) *Transport {
	t := &Transport{
		base:          cmp.Or(base, http.DefaultTransport),
		maxEntries:    DefaultMaxEntries,
		maxEntryBytes: DefaultMaxEntryBytes,
		maxTotalBytes: DefaultMaxTotalBytes,
		entries:       make(map[string]*list.Element),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Only GET is revalidated. A write has no cacheable representation, and
	// a caller that set its own If-None-Match is doing conditional requests
	// deliberately — stepping on that would change what it observes.
	if t.maxEntries <= 0 || r.Method != http.MethodGet || r.Header.Get("If-None-Match") != "" {
		return t.base.RoundTrip(r)
	}

	key := r.URL.String() + "\x00" + r.Header.Get("Accept")
	cached, ok := t.get(key)
	if !ok {
		resp, err := t.base.RoundTrip(r)
		if err != nil {
			return resp, err
		}
		t.remember(key, resp)
		mark(resp, "miss")
		return resp, nil
	}

	// Clone rather than mutate: the caller owns its request, and a header set
	// here would otherwise outlive this call and leak into a retry.
	probe := r.Clone(r.Context())
	probe.Header.Set("If-None-Match", cached.etag)

	resp, err := t.base.RoundTrip(probe)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusNotModified {
		// The representation moved, or the request failed. Either way the
		// remembered body no longer describes this URL.
		t.forget(key)
		t.remember(key, resp)
		mark(resp, "changed")
		return resp, nil
	}

	// Drain and close so the connection returns to the pool; the 304 carries
	// no body worth reading, only headers.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	replayed := cached.replay(r, resp)
	mark(replayed, "hit")
	return replayed, nil
}

// mark counts the revalidation outcome and tags the response with it, after
// remember has copied the headers, so the tag is never stored and replayed.
// The instrumented transport above (httpmetrics) logs the tag as the
// github_api_call cache field and removes it: replay turns a 304
// into a 200, so without it a hit is indistinguishable from a paid read.
func mark(resp *http.Response, result string) {
	mRequests.WithLabelValues(result).Inc()
	if resp.Header == nil {
		resp.Header = make(http.Header, 1)
	}
	resp.Header.Set(httpmetrics.CacheResultHeader, result)
}

// replay rebuilds the remembered response as the 200 it was, carrying this
// exchange's fresh headers (see freshHeaders) over the remembered ones.
func (e entry) replay(r *http.Request, notModified *http.Response) *http.Response {
	header := e.header.Clone()
	for _, name := range freshHeaders {
		if v, ok := notModified.Header[name]; ok {
			header[name] = v
		}
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         notModified.Proto,
		ProtoMajor:    notModified.ProtoMajor,
		ProtoMinor:    notModified.ProtoMinor,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       r,
	}
}

// remember stores a cacheable response, replacing resp.Body with an
// equivalent reader so the caller still sees the whole thing.
//
// Only a 200 carrying an ETag is worth remembering: without the validator
// there is nothing to revalidate against, and a non-200 has no representation
// this package should serve later.
func (t *Transport) remember(key string, resp *http.Response) {
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != http.StatusOK || etag == "" || resp.Body == nil {
		return
	}

	// A body larger than the total budget could only be stored by exceeding
	// it, so the total caps the per-entry limit too.
	limit := min(t.maxEntryBytes, t.maxTotalBytes)

	// Read one byte past the cap so an oversized body is detected without
	// buffering it whole, then hand the caller back everything either way:
	// what was read, followed by whatever remains unread.
	head, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		// The caller's read will fail the same way; leave the body as it is
		// rather than masking the error behind a partial replacement.
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), errReader{err}))
		return
	}
	if len(head) > limit {
		rest := resp.Body
		resp.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(head), rest), closer: rest}
		return
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(head))

	t.put(key, entry{etag: etag, header: resp.Header.Clone(), body: head})
}

// get returns the entry for key and marks it most recently used: a
// revalidation is a use whether it ends in a hit or replaces the body.
func (t *Transport) get(key string) (entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.entries[key]
	if !ok {
		return entry{}, false
	}
	t.recency.MoveToFront(el)
	return el.Value.(*slot).entry, true
}

func (t *Transport) forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.entries[key]; ok {
		t.drop(el)
	}
}

// drop removes one entry and its bytes. The caller holds t.mu.
func (t *Transport) drop(el *list.Element) {
	s := t.recency.Remove(el).(*slot)
	t.bytes -= len(s.body)
	delete(t.entries, s.key)
}

// put stores an entry as the most recently used, first evicting the least
// recently used entries until both bounds leave room for it. Recency rather
// than arbitrary order matters because one admitted body can be as large as
// hundreds of the small reads, such as a pull request GET, that a reconciler
// repeats on every pass; those stay at the front and survive. remember never
// passes a body larger than maxTotalBytes, so the loop always ends with room.
func (t *Transport) put(key string, e entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Two concurrent misses on one URL both store; count the body once.
	if el, ok := t.entries[key]; ok {
		t.drop(el)
	}
	for len(t.entries) >= t.maxEntries || t.bytes+len(e.body) > t.maxTotalBytes {
		t.drop(t.recency.Back())
		mEvictions.Inc()
	}
	t.entries[key] = t.recency.PushFront(&slot{key: key, entry: e})
	t.bytes += len(e.body)
}

// errReader yields a fixed error, so a body whose read failed mid-way reports
// that failure to the caller rather than looking truncated-but-complete.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// joinedBody re-presents an oversized body as the bytes already read followed
// by the rest, while still closing the original.
type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *joinedBody) Close() error { return b.closer.Close() }
