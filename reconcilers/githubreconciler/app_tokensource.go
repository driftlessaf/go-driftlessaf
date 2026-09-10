/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"fmt"
	"net/http"
	"os"
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
// Construct one with NewApp and use its methods.
type App struct {
	atr    *ghinstallation.AppsTransport
	signer ghinstallation.Signer
	client *github.Client
	mu     sync.RWMutex
	cache  map[string]int64
	sf     singleflight.Group
}

// NewApp creates an App from a key URI — gcpkms:// for a Cloud KMS key
// version, or file:// for a local PEM-encoded private key. The returned App
// caches installation ID lookups for the lifetime of the instance.
func NewApp(ctx context.Context, appID int64, keyURI string) (*App, error) {
	signer, err := newSigner(ctx, keyURI)
	if err != nil {
		return nil, err
	}
	atr, err := ghinstallation.NewAppsTransportWithOptions(http.DefaultTransport, appID, ghinstallation.WithSigner(signer))
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport: %w", err)
	}
	client, err := github.NewClient(github.WithTransport(atr))
	if err != nil {
		return nil, fmt.Errorf("create github client: %w", err)
	}
	return &App{
		atr:    atr,
		signer: signer,
		client: client,
		cache:  make(map[string]int64),
	}, nil
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

// LookupInstallID returns the GitHub App installation ID for org. Results are
// cached for the lifetime of the App. Concurrent lookups for the same org are
// coalesced into a single GitHub API call.
func (a *App) LookupInstallID(ctx context.Context, org string) (int64, error) {
	a.mu.RLock()
	id, ok := a.cache[org]
	a.mu.RUnlock()
	if ok {
		return id, nil
	}

	v, err, _ := a.sf.Do(org, func() (any, error) {
		id, err := appLookupInstallID(ctx, a.client, org)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.cache[org] = id
		a.mu.Unlock()
		return id, nil
	})
	if err != nil {
		return 0, err
	}
	return v.(int64), nil
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
	return a.InstallationTokenSource(ctx, installID, repo), nil
}

// InstallationTokenSource returns a token source minting installation tokens
// for the given installation ID, scoped to repo when non-empty. Unlike
// [App.TokenSourceFunc] it performs no org→installation lookup: it is for
// callers that already hold the authoritative installation ID (e.g. from an
// account-association record), so no GitHub API call is spent — or trusted —
// resolving an org login to an installation.
func (a *App) InstallationTokenSource(ctx context.Context, installID int64, repo string) oauth2.TokenSource {
	itr := ghinstallation.NewFromAppsTransport(a.atr, installID)
	if repo != "" {
		itr.InstallationTokenOptions = &github.InstallationTokenOptions{
			Repositories: []string{repo},
		}
	}
	return &appTokenSource{ctx: ctx, itr: itr}
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
	ctx context.Context
	itr *ghinstallation.Transport
}

func (ts *appTokenSource) Token() (*oauth2.Token, error) {
	tok, err := ts.itr.Token(ts.ctx)
	if err != nil {
		return nil, err
	}
	expiresAt, _, err := ts.itr.Expiry()
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{
		AccessToken: tok,
		TokenType:   "Bearer",
		Expiry:      expiresAt,
	}, nil
}

// appLookupInstallID returns the GitHub App installation ID for org by walking
// the app's installation list.
func appLookupInstallID(ctx context.Context, client *github.Client, org string) (int64, error) {
	page := 1
	for page != 0 {
		installs, resp, err := client.Apps.ListInstallations(ctx, &github.ListOptions{
			Page:    page,
			PerPage: 100,
		})
		if err != nil {
			return 0, err
		}
		for _, install := range installs {
			if install.Account.GetLogin() == org {
				return install.GetID(), nil
			}
		}
		page = resp.NextPage
	}
	return 0, fmt.Errorf("no GitHub App installation found for org %q", org)
}
