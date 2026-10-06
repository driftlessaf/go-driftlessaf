/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/modules/github-bots/sdk"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// TokenSourceFunc is a function that creates an OAuth2 token source for a given org/repo.
//
// When a ClientCache calls it, ctx carries the cache's eviction hook for the
// org. An App token source built from that ctx (App.RepoTokenSource,
// App.TokenSourceFunc) inherits the hook and runs it when its installation is
// gone, so a custom TokenSourceFunc that builds App sources should build them
// from the ctx it was given.
type TokenSourceFunc func(ctx context.Context, org, repo string) (oauth2.TokenSource, error)

// DefaultClientCacheSize is how many (org, repo) entries — a token source and
// the client built on it — a ClientCache keeps before evicting the least
// recently used.
//
// An evicted entry costs one installation-token mint the next time its key
// is reconciled and, with WithConditionalRequests, the client's remembered
// ETags. A kept entry costs the client and its token, plus whatever its
// conditional-request cache holds. 512 comfortably covers the repositories a
// reconciler works through within a token's hour, including one subscribed
// to every installation of a shared App, so eviction stays rare; its job is
// to stop a long-lived process accumulating one client per repository it has
// ever seen.
//
// Worst case, with WithConditionalRequests on, each client's cache can hold
// up to condcache.DefaultMaxTotalBytes (32 MiB) of bodies, so the bound on
// that memory is this size times that figure (16 GiB at the defaults). Real
// caches hold what the reconciler re-reads, far below the cap, but a
// reconciler that enables conditional requests across many repositories
// should size the two bounds together.
const DefaultClientCacheSize = 512

// ClientCacheOption configures a ClientCache.
type ClientCacheOption func(*clientCacheOptions)

type clientCacheOptions struct {
	size int
}

// WithClientCacheSize overrides DefaultClientCacheSize. A non-positive size
// keeps the default.
func WithClientCacheSize(n int) ClientCacheOption {
	return func(o *clientCacheOptions) { o.size = cmp.Or(max(n, 0), DefaultClientCacheSize) }
}

// ClientCache manages GitHub clients for multiple org/repo combinations. It is
// safe for concurrent use.
//
// Creating an entry runs outside any cache-wide lock and is coalesced per
// (org, repo), so a slow installation lookup for one org delays only callers
// for that org. Each caller waits on its own ctx: a caller whose ctx ends
// first gets ctx.Err(), and the creation carries on for the others.
type ClientCache struct {
	tokenSourceFunc TokenSourceFunc
	entries         *lruCache[string, *cacheEntry]
	sf              singleflight.Group
	// seq stamps each entry at creation. An eviction for an entry whose
	// installation ID is unknown removes only entries at or below its stamp
	// (see cacheEntry.staleAfter).
	seq atomic.Uint64

	// installIDFunc resolves an org to its GitHub App installation ID. It is set
	// by AppMain to the underlying App's (cached) LookupInstallID, so callers can
	// reuse the same installation-ID resolution the token minting already does,
	// rather than constructing a second App. It is nil for non-App entrypoints
	// (e.g. Octo STS via RepoMain/OrgMain), where LookupInstallID returns an error.
	//
	// When set, creating an entry resolves the org through it first, on the
	// caller's context values but detached from its cancellation (the
	// creation is shared by every caller for the key), bounded by the App's
	// lookup timeout; the token source built afterwards finds the ID already
	// cached.
	installIDFunc func(ctx context.Context, org string) (int64, error)

	// wrapTransport, when set, wraps each client's authenticated transport
	// before the client is built. Set by WithConditionalRequests; nil leaves
	// the transport as it was.
	//
	// It is applied PER CLIENT, which for a response cache is the whole
	// safety argument: this cache is keyed by (org, repo), so a wrapper that
	// remembers response bodies can only ever serve them back to the
	// credentials that fetched them.
	wrapTransport func(http.RoundTripper) http.RoundTripper
}

// cacheEntry is one (org, repo)'s token source and, once Get has asked for
// it, the client built on that source. Keeping both in one LRU entry evicts
// them together.
type cacheEntry struct {
	seq uint64
	// installID is the installation tokenSource mints for, when it says
	// (installationBound); 0 when it does not.
	installID   int64
	tokenSource oauth2.TokenSource
	client      atomic.Pointer[github.Client]
}

// NewClientCache creates a new client cache with the provided token source function.
func NewClientCache(tokenSourceFunc TokenSourceFunc, opts ...ClientCacheOption) *ClientCache {
	o := clientCacheOptions{size: DefaultClientCacheSize}
	for _, opt := range opts {
		opt(&o)
	}
	return &ClientCache{
		tokenSourceFunc: tokenSourceFunc,
		entries:         newLRU[string, *cacheEntry](o.size),
	}
}

// clientCacheKey returns the cache key for an org/repo combination.
// GitHub logins are case-insensitive, so the org part is folded: every
// spelling of an org shares its entries, and evictOrg finds them all.
func clientCacheKey(org, repo string) string {
	return fmt.Sprintf("%s/%s", strings.ToLower(org), repo)
}

// LookupInstallID returns the GitHub App installation ID for org, reusing the
// underlying App's cached lookup (the same one used to mint installation
// tokens). It returns an error when the cache was not created from a GitHub App
// entrypoint (AppMain), since only then is an App available to resolve it.
func (cc *ClientCache) LookupInstallID(ctx context.Context, org string) (int64, error) {
	if cc.installIDFunc == nil {
		return 0, fmt.Errorf("installation ID lookup is not configured: the client cache was not created from a GitHub App entrypoint")
	}
	return cc.installIDFunc(ctx, org)
}

// Get returns a GitHub client for the given org/repo, creating one if needed.
func (cc *ClientCache) Get(ctx context.Context, org, repo string) (*github.Client, error) {
	key := clientCacheKey(org, repo)
	if e, ok := cc.entries.get(key); ok {
		if client := e.client.Load(); client != nil {
			clog.DebugContext(ctx, "Using cached GitHub client", "org", org, "repo", repo)
			return client, nil
		}
	}

	return coalesce(ctx, &cc.sf, "client:"+key, func(ctx context.Context) (*github.Client, error) {
		e, err := cc.entryFor(ctx, org, repo)
		if err != nil {
			return nil, fmt.Errorf("creating token source: %w", err)
		}
		if client := e.client.Load(); client != nil {
			return client, nil
		}

		// Build the client through the SDK primitive so transport instrumentation
		// (httpmetrics) stays consistent with bots constructed via
		// sdk.NewGitHubClient / sdk.NewInstallationClient.
		//
		// Any wrapper sits BELOW httpmetrics, which sdk.NewClient puts outermost.
		// A conditional-request cache therefore still shows up as a request in
		// the metrics and the access log, but as the 200 it replays, not the 304
		// GitHub answered. condcache tags each response with its outcome
		// (httpmetrics.CacheResultHeader), which the access log records as its
		// cache field; that field, not status_code, tells a revalidation hit
		// from a paid read.
		transport := installationTransport(ctx, e.tokenSource, func() { cc.evictOrg(org, e.seq, e.installID) })
		if cc.wrapTransport != nil {
			transport = cc.wrapTransport(transport)
		}
		client := sdk.NewClient(transport)
		e.client.Store(client)

		clog.InfoContext(ctx, "Created new GitHub client for repository", "org", org, "repo", repo)
		return client, nil
	})
}

// TokenSourceFor returns an OAuth2 token source for the given org/repo combination.
// This allows callers that need raw token sources (e.g., for git clone operations)
// to reuse the same token source function that backs the client cache.
//
// The returned source is the one the TokenSourceFunc built, unwrapped, so
// callers may type-assert it. When GitHub answers 404 to a mint for its
// installation (the App was uninstalled, or reinstalled under a new ID) —
// whether through a client from Get or through a source a caller took from
// here, when the TokenSourceFunc is an App's — the org's cached clients and
// token sources bound to that installation are evicted (or, for sources that
// do not say which installation they mint for, those created at or before the
// failing one), so the next Get or TokenSourceFor re-resolves it.
func (cc *ClientCache) TokenSourceFor(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
	e, err := cc.entryFor(ctx, org, repo)
	if err != nil {
		return nil, err
	}
	return e.tokenSource, nil
}

// entryFor returns org/repo's cache entry, creating it with a fresh token
// source if needed.
func (cc *ClientCache) entryFor(ctx context.Context, org, repo string) (*cacheEntry, error) {
	key := clientCacheKey(org, repo)
	if e, ok := cc.entries.get(key); ok {
		clog.DebugContext(ctx, "Using cached GitHub token source", "org", org, "repo", repo)
		return e, nil
	}

	return coalesce(ctx, &cc.sf, "token:"+key, func(ctx context.Context) (*cacheEntry, error) {
		if e, ok := cc.entries.get(key); ok {
			return e, nil
		}

		if cc.installIDFunc != nil {
			if _, err := cc.installIDFunc(ctx, org); err != nil {
				return nil, err
			}
		}

		// Use context.Background() because token sources capture the context for
		// later Token refreshes. Binding a cached source to a request context can
		// poison the cache after that request is canceled, and can leak request-
		// scoped deadlines or values into later refreshes. The installation
		// lookup that needs bounding ran above. The one value carried is the
		// hook an App token source runs when its installation is gone.
		seq := cc.seq.Add(1)
		inner, err := cc.tokenSourceFunc(withInstallationGoneHook(context.Background(), func(id int64) { cc.evictOrg(org, seq, id) }), org, repo)
		if err != nil {
			return nil, err
		}
		e := &cacheEntry{seq: seq, tokenSource: inner}
		if b, ok := inner.(installationBound); ok {
			e.installID = b.installationID()
		}
		cc.entries.add(key, e)

		clog.InfoContext(ctx, "Created new GitHub token source for repository", "org", org, "repo", repo)
		return e, nil
	})
}

// evictOrg drops org's cached clients and token sources that a failure on
// the entry stamped seq, bound to installation installID, shows to be stale
// (see cacheEntry.staleAfter).
func (cc *ClientCache) evictOrg(org string, seq uint64, installID int64) {
	prefix := strings.ToLower(org) + "/"
	cc.entries.removeIf(func(key string, e *cacheEntry) bool {
		return strings.HasPrefix(key, prefix) && e.staleAfter(seq, installID)
	})
}

// staleAfter reports whether a failure that found the installation gone, on
// the entry stamped seq and bound to installID, makes e stale.
//
// When both installation IDs are known, e is stale exactly when it is bound
// to the same installation. Creation order cannot decide that: an entry can
// resolve the old ID, stall, and be stamped after a sibling that resolved
// the replacement installation, and its late 404 must not evict that
// sibling. When either ID is unknown (a TokenSourceFunc that does not expose
// its installation), e is stale if it was created at or before the failing
// entry, so a late failure on an old entry cannot evict one built after it.
func (e *cacheEntry) staleAfter(seq uint64, installID int64) bool {
	if e.installID != 0 && installID != 0 {
		return e.installID == installID
	}
	return e.seq <= seq
}

// Clear removes all cached clients and token sources.
func (cc *ClientCache) Clear() {
	cc.entries.purge()
}

// installationTransport is the authenticated transport under a client from
// Get. It differs from oauth2.NewClient's in two ways, both so that an
// uninstall is noticed on the next call rather than when an hour-long token
// expires:
//
//   - a 401 from GitHub on a request expires the token that request carried,
//     so the next request mints a fresh one (see expireOn401);
//   - a mint answered with 404 runs evict (see evictOnGone).
//
// The 401 itself evicts nothing: it is the mint that follows which says
// whether the installation is gone. The base transport is taken from ctx the
// way oauth2.NewClient takes it (oauth2.HTTPClient).
func installationTransport(ctx context.Context, src oauth2.TokenSource, evict func()) http.RoundTripper {
	base := http.DefaultTransport
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && c != nil && c.Transport != nil {
		base = c.Transport
	}
	reuse := &expirableTokenSource{src: &evictOnGone{inner: src, evict: evict}}
	return &oauth2.Transport{
		Source: reuse,
		Base:   &expireOn401{base: base, ts: reuse},
	}
}

// tokenExpirer is a token source that can drop a cached token it handed out,
// so the next Token fetches a new one.
type tokenExpirer interface {
	expireToken(token string)
}

// expirableTokenSource reuses a token until it nears expiry, as
// oauth2.ReuseTokenSource does, and can also drop it early.
type expirableTokenSource struct {
	src oauth2.TokenSource
	// mu guards tok and lastForced (when expireToken last dropped a
	// token), and is held across the call down to src so two expiries
	// cannot interleave with a fetch.
	mu         sync.Mutex
	tok        *oauth2.Token
	lastForced time.Time
}

var _ tokenExpirer = (*expirableTokenSource)(nil)

func (ts *expirableTokenSource) Token() (*oauth2.Token, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.tok.Valid() {
		return ts.tok, nil
	}
	tok, err := ts.src.Token()
	if err != nil {
		return nil, err
	}
	ts.tok = tok
	return tok, nil
}

// expireToken drops token if it is the cached one and no forced drop
// happened in the last minForcedRemintInterval, and passes the request down
// so a source that caches below (an App installation source) drops it too.
// The first 401 therefore re-mints at once however young the token, and
// forced refetches stay capped at one per source per interval whatever the
// 401s are about.
func (ts *expirableTokenSource) expireToken(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.tok == nil || ts.tok.AccessToken != token || recentlyForced(ts.lastForced) {
		return
	}
	ts.tok = nil
	ts.lastForced = time.Now()
	if e, ok := ts.src.(tokenExpirer); ok {
		e.expireToken(token)
	}
}

// expireOn401 sits below oauth2.Transport, where each request carries its
// token, and expires that token when GitHub answers 401 Bad credentials.
type expireOn401 struct {
	base http.RoundTripper
	ts   tokenExpirer
}

func (t *expireOn401) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			t.ts.expireToken(token)
		}
	}
	return resp, err
}

// evictOnGone wraps a cached token source and runs evict when GitHub answers
// 404 to a mint for its installation, so the cache stops handing out a
// client bound to an installation ID that no longer exists. It covers any
// TokenSourceFunc whose errors carry ghinstallation's HTTPError, not only an
// App's.
type evictOnGone struct {
	inner oauth2.TokenSource
	evict func()
}

var _ tokenExpirer = (*evictOnGone)(nil)

func (ts *evictOnGone) Token() (*oauth2.Token, error) {
	tok, err := ts.inner.Token()
	if err != nil && isInstallationGone(err) {
		ts.evict()
	}
	return tok, err
}

func (ts *evictOnGone) expireToken(token string) {
	if e, ok := ts.inner.(tokenExpirer); ok {
		e.expireToken(token)
	}
}

// coalesce runs create once per key across concurrent callers. create runs
// on a context that keeps ctx's values but not its cancellation, so the
// caller that started it giving up does not fail the others; each caller
// stops waiting when its own ctx is done.
//
// A panic in create is returned as an error: singleflight re-raises a
// DoChan panic on a goroutine of its own, where no recovery interceptor can
// reach it, and create runs caller-supplied token source functions.
func coalesce[T any](ctx context.Context, sf *singleflight.Group, key string, create func(context.Context) (T, error)) (T, error) {
	ch := sf.DoChan(key, func() (v any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("creating %s: panic: %v\n%s", key, r, debug.Stack())
			}
		}()
		return create(context.WithoutCancel(ctx))
	})
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			var zero T
			return zero, res.Err
		}
		return res.Val.(T), nil
	}
}
