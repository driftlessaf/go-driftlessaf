/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsign

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	gogit "github.com/go-git/go-git/v5"
	"github.com/sigstore/cosign/v2/pkg/providers"
	"github.com/sigstore/gitsign/pkg/fulcio"
	"github.com/sigstore/gitsign/pkg/gitsign"
	"github.com/sigstore/gitsign/pkg/rekor"
	"github.com/sigstore/sigstore/pkg/oauthflow"
	"golang.org/x/oauth2"

	// Sigstore auth providers - targeting Cloud Run so only include Google provider
	_ "github.com/sigstore/cosign/v2/pkg/providers/google"
)

func NewSigner(ctx context.Context) (gogit.Signer, error) {
	if !providers.Enabled(ctx) {
		return nil, fmt.Errorf("no sigstore providers enabled")
	}

	fulcio, err := fulcio.NewClient("https://fulcio.sigstore.dev", fulcio.OIDCOptions{
		ClientID: "sigstore",
		Issuer:   "https://oauth2.sigstore.dev/auth",
		TokenGetter: &providerTokenGetter{
			ctx:      ctx,
			audience: "sigstore",
		},
	})
	if err != nil {
		return nil, err
	}
	rekor, err := rekor.NewWithOptions(ctx, "https://rekor.sigstore.dev")
	if err != nil {
		return nil, err
	}
	return gitsign.NewSigner(ctx, fulcio, rekor)
}

type providerTokenGetter struct {
	ctx      context.Context
	audience string
}

func (p *providerTokenGetter) GetIDToken(_ *oidc.Provider, _ oauth2.Config) (*oauthflow.OIDCIDToken, error) {
	token, err := providers.Provide(p.ctx, p.audience)
	if err != nil {
		return nil, fmt.Errorf("provide token: %w", err)
	}
	payload, err := decodeJWTPayload(token)
	if err != nil {
		return nil, err
	}
	subject, err := oauthflow.SubjectFromUnverifiedToken(payload)
	if err != nil {
		return nil, fmt.Errorf("extract subject: %w", err)
	}
	return &oauthflow.OIDCIDToken{RawString: token, Subject: subject}, nil
}

func decodeJWTPayload(token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	return payload, nil
}
