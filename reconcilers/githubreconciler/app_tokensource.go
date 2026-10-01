/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"github.com/bradleyfalzon/ghinstallation/v2"
	jwt "github.com/golang-jwt/jwt/v4"
	"github.com/google/go-github/v88/github"
	"github.com/octo-sts/app/pkg/gcpkms"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// App holds the transport and installation ID cache for a GitHub App.
// Construct one with NewApp and use its methods. An App is safe for
// concurrent use.
type App struct {
	atr    *ghinstallation.AppsTransport
	signer ghinstallation.Signer
	client *github.Client

	// cache holds resolved installation IDs. It only ever holds orgs the App
	// is installed on, so it is bounded by the App's installation count and
	// needs no LRU. An entry is dropped when minting a token against it
	// answers 404 (see forgetInstallID), which is how a reinstall is picked up.
	mu    sync.RWMutex
	cache map[string]int64

	// negative remembers owners GitHub says the App is not installed on
	// (ErrNoInstallation) for negativeTTL, so a stream of events for such an
	// owner costs one lookup per TTL rather than one per event. Only that
	// answer is remembered: a 5xx, a rate limit, a timeout or a refused App
	// JWT says nothing about the installation and is retried. It is keyed by
	// caller-supplied owners, so unlike cache it is bounded.
	negative *lruCache[string, negativeLookup]
	sf       singleflight.Group

	lookupTimeout time.Duration
	negativeTTL   time.Duration
}

// ErrNoInstallation is returned (wrapped) by LookupInstallID when GitHub
// reports that the App has no installation on the owner, as an organization
// or as a user. Callers may test for it with errors.Is.
var ErrNoInstallation = errors.New("no GitHub App installation")

// negativeLookup is a remembered ErrNoInstallation answer.
type negativeLookup struct {
	err     error
	expires time.Time
}

const (
	// DefaultInstallLookupTimeout bounds one owner→installation resolution
	// (at most two App-JWT calls: the organization, then the user endpoint).
	// It exists so a hung GitHub cannot pin a lookup, and every caller
	// coalesced onto it, forever.
	DefaultInstallLookupTimeout = 30 * time.Second

	// DefaultInstallLookupNegativeTTL is how long an ErrNoInstallation
	// answer is remembered. Short, because the common cause of a miss is an
	// install that is happening right now; long enough that a burst of
	// events for an owner the App is not installed on costs one lookup.
	DefaultInstallLookupNegativeTTL = 30 * time.Second

	// negativeLookupEntries bounds the failed-lookup cache. Owners come from
	// workqueue keys, so the set is not bounded by the installation count.
	negativeLookupEntries = 1024
)

// AppOption configures an App.
type AppOption func(*App)

// WithInstallLookupTimeout overrides DefaultInstallLookupTimeout. A
// non-positive value keeps the default.
func WithInstallLookupTimeout(d time.Duration) AppOption {
	return func(a *App) { a.lookupTimeout = cmp.Or(max(d, 0), DefaultInstallLookupTimeout) }
}

// WithInstallLookupNegativeTTL overrides DefaultInstallLookupNegativeTTL. A
// non-positive value keeps the default.
func WithInstallLookupNegativeTTL(d time.Duration) AppOption {
	return func(a *App) { a.negativeTTL = cmp.Or(max(d, 0), DefaultInstallLookupNegativeTTL) }
}

// NewApp creates an App from a key URI — gcpkms:// for a Cloud KMS key
// version, or file:// for a local PEM-encoded private key. The returned App
// caches installation ID lookups for the lifetime of the instance, dropping
// an entry when a mint against it answers 404.
func NewApp(ctx context.Context, appID int64, keyURI string, opts ...AppOption) (*App, error) {
	signer, err := newSigner(ctx, keyURI)
	if err != nil {
		return nil, err
	}
	return newApp(appID, signer, http.DefaultTransport, "", opts...)
}

// newApp builds an App over tr. An empty baseURL keeps api.github.com; tests
// point it at a fake.
func newApp(appID int64, signer ghinstallation.Signer, tr http.RoundTripper, baseURL string, opts ...AppOption) (*App, error) {
	atr, err := ghinstallation.NewAppsTransportWithOptions(tr, appID, ghinstallation.WithSigner(signer))
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport: %w", err)
	}
	clientOpts := []github.ClientOptionsFunc{github.WithTransport(atr)}
	if baseURL != "" {
		atr.BaseURL = baseURL
		clientOpts = append(clientOpts, github.WithURLs(new(baseURL+"/"), nil))
	}
	client, err := github.NewClient(clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("create github client: %w", err)
	}
	a := &App{
		atr:           atr,
		signer:        signer,
		client:        client,
		cache:         make(map[string]int64),
		negative:      newLRU[string, negativeLookup](negativeLookupEntries),
		lookupTimeout: DefaultInstallLookupTimeout,
		negativeTTL:   DefaultInstallLookupNegativeTTL,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// ID returns the GitHub App ID.
func (a *App) ID() int64 {
	return a.atr.AppID()
}

// AppTokenSource returns a token source minting the App's own JWTs — the
// credential GitHub requires on the endpoints that describe the App and its
// installations, where an installation token is refused. Each JWT is
// short-lived (the same window ghinstallation's AppsTransport uses) and
// reused until it nears expiry, so a burst of App-authenticated calls costs
// one signature, not one per call. For go-github callers, [App.Client]
// signs the same way per request.
func (a *App) AppTokenSource() oauth2.TokenSource {
	return oauth2.ReuseTokenSourceWithExpiry(nil, &appJWTSource{appID: a.ID(), signer: a.signer}, appJWTRefreshBefore)
}

// Client returns a GitHub client authenticated as the app using a JWT (not an
// installation token). Use this for app-level API calls such as listing
// installations and their repositories. Unlike the clients vended by
// ClientCache, this client is not scoped to a specific installation.
func (a *App) Client() *github.Client {
	return a.client
}

// LookupInstallID returns the GitHub App installation ID for org, which may
// be an organization or a user. Logins are case-insensitive, so every
// spelling of one shares a cache entry; a string that is not a GitHub login
// is ErrNoInstallation without a request. Results are cached for the lifetime of the
// App, until minting a token against the cached ID answers 404 (the App was
// uninstalled, or reinstalled under a new ID). When GitHub reports no
// installation the error wraps ErrNoInstallation and is remembered for the
// negative TTL; other failures are not remembered. Concurrent lookups for the
// same org are coalesced into one.
//
// The lookup runs detached from ctx's cancellation, so one caller giving up
// does not fail the others coalesced onto it, and is bounded by the lookup
// timeout instead. ctx still bounds how long this caller waits: when it is
// done first, LookupInstallID returns ctx.Err().
func (a *App) LookupInstallID(ctx context.Context, org string) (int64, error) {
	if !validOwner(org) {
		// Not a GitHub login, so it cannot name an installation. Refused
		// before any request, so an owner from a workqueue key never reaches
		// a URL path unchecked.
		return 0, fmt.Errorf("%w for %q: not a valid GitHub login", ErrNoInstallation, org)
	}
	key := strings.ToLower(org)

	a.mu.RLock()
	id, ok := a.cache[key]
	a.mu.RUnlock()
	if ok {
		return id, nil
	}
	if neg, ok := a.negative.get(key); ok {
		if time.Now().Before(neg.expires) {
			return 0, neg.err
		}
		a.negative.remove(key)
	}

	ch := a.sf.DoChan(key, func() (any, error) {
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.lookupTimeout)
		defer cancel()
		id, err := appLookupInstallID(lookupCtx, a.client, org)
		if err != nil {
			if errors.Is(err, ErrNoInstallation) {
				a.negative.add(key, negativeLookup{err: err, expires: time.Now().Add(a.negativeTTL)})
			}
			return nil, err
		}
		a.mu.Lock()
		a.cache[key] = id
		a.mu.Unlock()
		return id, nil
	})
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return 0, res.Err
		}
		return res.Val.(int64), nil
	}
}

// forgetInstallID drops org's cached installation ID if it is still id, so
// the next lookup asks GitHub again. Comparing first keeps a late failure on
// a stale token source from discarding an ID a concurrent lookup has already
// refreshed.
func (a *App) forgetInstallID(org string, id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := strings.ToLower(org)
	if cached, ok := a.cache[key]; ok && cached == id {
		delete(a.cache, key)
	}
}

// installationGoneHookKey carries a func run, with the gone installation's
// ID, when a token source built by RepoTokenSource finds its installation
// gone. ClientCache sets it on the context it passes its TokenSourceFunc, so
// a 404 on any of its sources — including one a caller took raw from
// TokenSourceFor — evicts the org's entries bound to that installation from
// the cache as well as from the App.
type installationGoneHookKey struct{}

func withInstallationGoneHook(ctx context.Context, hook func(installID int64)) context.Context {
	return context.WithValue(ctx, installationGoneHookKey{}, hook)
}

// TokenSourceFunc returns a TokenSourceFunc that mints installation tokens
// scoped to the requested org/repo.
func (a *App) TokenSourceFunc() TokenSourceFunc {
	return func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
		return a.RepoTokenSource(ctx, org, repo)
	}
}

// OrgTokenSource returns a token source minting an installation token scoped
// to every repo in org. Prefer this over RepoTokenSource when a token will be
// reused across several repos in the same org, since it saves minting one
// token per repo.
func (a *App) OrgTokenSource(ctx context.Context, org string) (oauth2.TokenSource, error) {
	return a.RepoTokenSource(ctx, org, "")
}

// RepoTokenSource returns a token source minting installation tokens scoped
// to org/repo, resolving org's installation ID first (see LookupInstallID).
func (a *App) RepoTokenSource(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
	installID, err := a.LookupInstallID(ctx, org)
	if err != nil {
		return nil, err
	}
	ts := a.installationTokenSource(ctx, installID, repo)
	// The ID came from the lookup cache, so a 404 minting against it means
	// the cache is stale: forget it so the next source re-resolves.
	hook, _ := ctx.Value(installationGoneHookKey{}).(func(int64))
	ts.onGone = func() {
		a.forgetInstallID(org, installID)
		if hook != nil {
			hook(installID)
		}
	}
	return ts, nil
}

// InstallationTokenSource returns a token source minting installation tokens
// for the given installation ID, scoped to repo when non-empty. Unlike
// [App.TokenSourceFunc] it performs no org→installation lookup: it is for
// callers that already hold the authoritative installation ID (e.g. from an
// account-association record), so no GitHub API call is spent — or trusted —
// resolving an org login to an installation.
func (a *App) InstallationTokenSource(ctx context.Context, installID int64, repo string) oauth2.TokenSource {
	return a.installationTokenSource(ctx, installID, repo)
}

func (a *App) installationTokenSource(ctx context.Context, installID int64, repo string) *appTokenSource {
	ts := &appTokenSource{ctx: ctx, atr: a.atr, installID: installID, repo: repo}
	ts.itr = ts.newTransport()
	return ts
}

// newSigner builds the App's JWT signer from a key URI: gcpkms:// (a Cloud
// KMS key version, signing remotely so the key never leaves KMS) or file://
// (a local PEM-encoded RSA private key, for development).
func newSigner(ctx context.Context, keyURI string) (ghinstallation.Signer, error) {
	scheme, rest, ok := strings.Cut(keyURI, "://")
	if !ok {
		return nil, fmt.Errorf("unsupported key URI %q: want gcpkms:// or file://", keyURI)
	}
	switch scheme {
	case "gcpkms":
		kmsClient, err := kms.NewKeyManagementClient(ctx)
		if err != nil {
			return nil, err
		}
		return gcpkms.New(ctx, kmsClient, rest)
	case "file":
		pemBytes, err := os.ReadFile(rest)
		if err != nil {
			return nil, fmt.Errorf("reading GitHub App key %q: %w", keyURI, err)
		}
		key, err := jwt.ParseRSAPrivateKeyFromPEM(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("parsing GitHub App key %q: %w", keyURI, err)
		}
		return ghinstallation.NewRSASigner(jwt.SigningMethodRS256, key), nil
	default:
		return nil, fmt.Errorf("unsupported key URI %q: want gcpkms:// or file://", keyURI)
	}
}

const (
	// appJWTLifetime is how long a minted App JWT is valid, measured from an
	// issued-at stamped slightly in the past to absorb clock skew between
	// here and GitHub — the same window ghinstallation's AppsTransport uses.
	appJWTLifetime = 2 * time.Minute
	appJWTSkew     = 30 * time.Second
	// appJWTRefreshBefore is how close to expiry a cached App JWT is replaced.
	appJWTRefreshBefore = 30 * time.Second
)

// appJWTSource mints App JWTs; wrap it in oauth2.ReuseTokenSourceWithExpiry
// (see App.AppTokenSource) so tokens are reused while valid.
type appJWTSource struct {
	appID  int64
	signer ghinstallation.Signer
}

func (s *appJWTSource) Token() (*oauth2.Token, error) {
	// GitHub rejects fractional timestamps, so truncate to whole seconds.
	iss := time.Now().Add(-appJWTSkew).Truncate(time.Second)
	exp := iss.Add(appJWTLifetime)
	signed, err := s.signer.Sign(&jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(iss),
		ExpiresAt: jwt.NewNumericDate(exp),
		Issuer:    strconv.FormatInt(s.appID, 10),
	})
	if err != nil {
		return nil, fmt.Errorf("signing GitHub App JWT: %w", err)
	}
	return &oauth2.Token{AccessToken: signed, TokenType: "Bearer", Expiry: exp}, nil
}

// appTokenSource adapts a *ghinstallation.Transport to oauth2.TokenSource.
type appTokenSource struct {
	ctx       context.Context
	atr       *ghinstallation.AppsTransport
	installID int64
	repo      string
	// onGone, when set, runs when GitHub answers 404 to a mint for the
	// installation (see isInstallationGone).
	onGone func()

	// mu guards itr, last and lastForced. itr caches the minted token until
	// near its expiry; expireToken replaces it to force the next Token to
	// mint, at lastForced. last is the token itr most recently handed out.
	mu         sync.Mutex
	itr        *ghinstallation.Transport
	last       string
	lastForced time.Time
}

// minForcedRemintInterval is the least time between two re-mints a 401
// forces on one source. A 401 that has nothing to do with the token (an
// endpoint that refuses installation tokens, a GitHub auth incident, a
// redirect to another host) would otherwise mint once per request; this caps
// forced re-mints at one per source per minute. The first 401 is never held
// back, so an uninstall or reinstall is noticed on the next call however
// recently the token was minted.
const minForcedRemintInterval = time.Minute

// recentlyForced reports whether a forced re-mint at last is still within
// minForcedRemintInterval.
func recentlyForced(last time.Time) bool {
	return !last.IsZero() && time.Since(last) < minForcedRemintInterval
}

var (
	_ tokenExpirer      = (*appTokenSource)(nil)
	_ installationBound = (*appTokenSource)(nil)
)

// installationBound is a token source that mints for one installation, and
// says which.
type installationBound interface {
	installationID() int64
}

func (ts *appTokenSource) installationID() int64 { return ts.installID }

func (ts *appTokenSource) newTransport() *ghinstallation.Transport {
	itr := ghinstallation.NewFromAppsTransport(ts.atr, ts.installID)
	if ts.repo != "" {
		itr.InstallationTokenOptions = &github.InstallationTokenOptions{
			Repositories: []string{ts.repo},
		}
	}
	return itr
}

func (ts *appTokenSource) Token() (*oauth2.Token, error) {
	ts.mu.Lock()
	itr := ts.itr
	ts.mu.Unlock()

	tok, err := itr.Token(ts.ctx)
	if err != nil {
		if ts.onGone != nil && isInstallationGone(err) {
			ts.onGone()
		}
		return nil, err
	}
	expiresAt, _, err := itr.Expiry()
	if err != nil {
		return nil, err
	}
	ts.mu.Lock()
	if ts.itr == itr {
		ts.last = tok
	}
	ts.mu.Unlock()
	return &oauth2.Token{
		AccessToken: tok,
		TokenType:   "Bearer",
		Expiry:      expiresAt,
	}, nil
}

// expireToken drops the cached installation token if it is still token and
// no forced re-mint happened in the last minForcedRemintInterval, so the next
// Token mints. Comparing first means a burst of requests refused with the
// same token forces one mint, not one per request.
func (ts *appTokenSource) expireToken(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.last == "" || ts.last != token || recentlyForced(ts.lastForced) {
		return
	}
	ts.itr = ts.newTransport()
	ts.last = ""
	ts.lastForced = time.Now()
}

// isInstallationGone reports whether err is GitHub answering 404 to an
// installation-token mint: the installation no longer exists, which is what
// an uninstall, or an uninstall followed by a reinstall under a new ID, looks
// like from the token endpoint. A 401 is not: it means the App's own JWT was
// refused (clock skew, a rotated key, an incident), which hits every org at
// once and says nothing about any one installation. Neither are transport
// errors or 5xx.
func isInstallationGone(err error) bool {
	httpErr, ok := errors.AsType[*ghinstallation.HTTPError](err)
	return ok && httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusNotFound
}

// appLookupInstallID resolves owner to the App's installation on it: the
// organization endpoint first, then, on a 404, the user endpoint. A 404 from
// both is an unambiguous "not installed" and wraps ErrNoInstallation; any
// other failure is returned as it is.
//
// An answer is accepted only when its account login is owner, compared as
// GitHub compares logins (case-insensitively); anything else is treated as
// not installed.
func appLookupInstallID(ctx context.Context, client *github.Client, owner string) (int64, error) {
	install, resp, err := client.Apps.GetOrganizationInstallation(ctx, owner)
	if err == nil {
		return acceptInstall(install, owner)
	}
	if !isNotFound(resp) {
		return 0, fmt.Errorf("finding GitHub App installation for organization %q: %w", owner, err)
	}
	install, resp, err = client.Apps.GetUserInstallation(ctx, owner)
	if err == nil {
		return acceptInstall(install, owner)
	}
	if !isNotFound(resp) {
		return 0, fmt.Errorf("finding GitHub App installation for user %q: %w", owner, err)
	}
	return 0, fmt.Errorf("%w for %q", ErrNoInstallation, owner)
}

func acceptInstall(install *github.Installation, owner string) (int64, error) {
	if login := install.GetAccount().GetLogin(); !strings.EqualFold(login, owner) {
		return 0, fmt.Errorf("%w for %q: GitHub answered for account %q", ErrNoInstallation, owner, login)
	}
	return install.GetID(), nil
}

// ownerPattern matches a GitHub login: an alphanumeric first character, then
// up to 38 alphanumerics, hyphens or underscores. Organization and ordinary
// user logins use only alphanumerics and hyphens; underscores appear in
// Enterprise Managed User logins ("handle_shortcode"), which can own an App
// installation. Nothing in the set is meaningful in a URL path.
var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)

func validOwner(owner string) bool {
	return ownerPattern.MatchString(owner)
}

func isNotFound(resp *github.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}
