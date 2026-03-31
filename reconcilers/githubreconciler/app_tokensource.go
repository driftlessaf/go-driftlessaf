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

	kms "cloud.google.com/go/kms/apiv1"
	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v84/github"
	"github.com/octo-sts/app/pkg/gcpkms"
	"golang.org/x/oauth2"
)

// NewAppClient creates a GitHub client authenticated as the app using a JWT
// (not an installation token). Use this for app-level API calls such as
// listing installations and their repositories. Unlike the clients vended by
// ClientCache, this client is not scoped to a specific installation.
func NewAppClient(ctx context.Context, appID int64, appKey string) (*github.Client, error) {
	atr, err := newAppTransport(ctx, appID, appKey)
	if err != nil {
		return nil, err
	}
	return github.NewClient(&http.Client{Transport: atr}), nil
}

// NewAppTokenSource creates a TokenSourceFunc backed by the GitHub App
// identified by appID. appKey must be a gcpkms:// URI pointing to the Cloud
// KMS crypto key version holding the app's RSA private key.
//
// The returned TokenSourceFunc resolves the installation ID for each org on
// first use and mints a scoped installation token for the requested org/repo.
func NewAppTokenSource(ctx context.Context, appID int64, appKey string) (TokenSourceFunc, error) {
	atr, err := newAppTransport(ctx, appID, appKey)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
		return newAppRepoTokenSource(ctx, atr, org, repo)
	}, nil
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

// newAppRepoTokenSource resolves the installation ID for org immediately and
// returns an oauth2.TokenSource scoped to org/repo.
func newAppRepoTokenSource(ctx context.Context, atr *ghinstallation.AppsTransport, org, repo string) (oauth2.TokenSource, error) {
	installID, err := appLookupInstallID(ctx, atr, org)
	if err != nil {
		return nil, err
	}

	itr := ghinstallation.NewFromAppsTransport(atr, installID)
	if repo != "" {
		itr.InstallationTokenOptions = &github.InstallationTokenOptions{
			Repositories: []string{repo},
		}
	}

	return &appTokenSource{ctx: ctx, itr: itr}, nil
}

// appLookupInstallID returns the GitHub App installation ID for org by walking
// the app's installation list.
func appLookupInstallID(ctx context.Context, atr *ghinstallation.AppsTransport, org string) (int64, error) {
	client := github.NewClient(&http.Client{Transport: atr})
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
