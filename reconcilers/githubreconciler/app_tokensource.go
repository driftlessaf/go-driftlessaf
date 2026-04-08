/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	kms "cloud.google.com/go/kms/apiv1"
	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v84/github"
	"github.com/octo-sts/app/pkg/gcpkms"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// App holds the transport and installation ID cache for a GitHub App.
// Construct one with NewApp and use its methods.
type App struct {
	atr    *ghinstallation.AppsTransport
	client *github.Client
	mu     sync.RWMutex
	cache  map[string]int64
	sf     singleflight.Group
}

// NewApp creates an App from a gcpkms:// key URI. The returned App caches
// installation ID lookups for the lifetime of the instance.
func NewApp(ctx context.Context, appID int64, keyURI string) (*App, error) {
	atr, err := newAppTransport(ctx, appID, keyURI)
	if err != nil {
		return nil, err
	}
	return &App{
		atr:    atr,
		client: github.NewClient(&http.Client{Transport: atr}),
		cache:  make(map[string]int64),
	}, nil
}

// ID returns the GitHub App ID.
func (a *App) ID() int64 {
	return a.atr.AppID()
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
		return a.newRepoTokenSource(ctx, org, repo)
	}
}

func (a *App) newRepoTokenSource(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
	installID, err := a.LookupInstallID(ctx, org)
	if err != nil {
		return nil, err
	}
	itr := ghinstallation.NewFromAppsTransport(a.atr, installID)
	if repo != "" {
		itr.InstallationTokenOptions = &github.InstallationTokenOptions{
			Repositories: []string{repo},
		}
	}
	return &appTokenSource{ctx: ctx, itr: itr}, nil
}

// newAppTransport creates a *ghinstallation.AppsTransport from a gcpkms:// key URI.
func newAppTransport(ctx context.Context, appID int64, keyURI string) (*ghinstallation.AppsTransport, error) {
	parts := strings.SplitN(keyURI, "://", 2)
	if len(parts) != 2 || parts[0] != "gcpkms" {
		return nil, fmt.Errorf("unsupported key URI %q: only gcpkms:// is supported", keyURI)
	}
	kmsClient, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, err
	}
	signer, err := gcpkms.New(ctx, kmsClient, parts[1])
	if err != nil {
		return nil, err
	}
	atr, err := ghinstallation.NewAppsTransportWithOptions(http.DefaultTransport, appID, ghinstallation.WithSigner(signer))
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport: %w", err)
	}
	return atr, nil
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
