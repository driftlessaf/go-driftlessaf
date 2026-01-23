/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"context"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

type gsaOIDCProvider struct {
	ts oauth2.TokenSource
}

func newGSAOIDCProvider(ctx context.Context, audience string) (*gsaOIDCProvider, error) {
	ts, err := idtoken.NewTokenSource(ctx, audience)
	if err != nil {
		return nil, err
	}
	return &gsaOIDCProvider{ts: oauth2.ReuseTokenSource(nil, ts)}, nil
}

func (p *gsaOIDCProvider) Enabled(context.Context) bool {
	return true
}

func (p *gsaOIDCProvider) Provide(ctx context.Context, _ string) (string, error) {
	tok, err := p.ts.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}
