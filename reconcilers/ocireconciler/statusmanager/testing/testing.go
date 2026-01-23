//go:build withauth

/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package testing provides test helpers for statusmanager.
package testing

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager"
	"chainguard.dev/sdk/auth"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/fulcio"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/stretchr/testify/require"
)

// chainctlOIDCProvider implements fulcio.OIDCProvider using chainctl auth token.
type chainctlOIDCProvider struct{}

func (p *chainctlOIDCProvider) Enabled(context.Context) bool {
	return true
}

func (p *chainctlOIDCProvider) Provide(ctx context.Context, _ string) (string, error) {
	cmd := exec.CommandContext(ctx, "chainctl", "auth", "token", "--audience", "sigstore")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get chainctl token: %w", err)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("chainctl token is empty")
	}

	return token, nil
}

// setupProviderAndIdentity creates a chainctl OIDC provider and extracts the
// signing identity from a token. This is a test helper that constructs the
// identity in the Chainguard-specific format (issuer/subject).
func setupProviderAndIdentity(t *testing.T, ctx context.Context) (fulcio.OIDCProvider, cosign.Identity) {
	t.Helper()

	// Create custom OIDC provider using chainctl
	provider := &chainctlOIDCProvider{}

	// Get a token to extract the signing identity
	token, err := provider.Provide(ctx, "sigstore")
	require.NoError(t, err, "failed to get token")

	// Extract issuer and subject from token to construct the correct identity
	issuer, subject, err := auth.ExtractIssuerAndSubject(token)
	require.NoError(t, err, "failed to extract issuer and subject")

	// For Chainguard tokens, the identity subject is issuer + "/" + subject
	identity := cosign.Identity{
		Subject: issuer + "/" + subject,
		Issuer:  issuer,
	}

	t.Logf("Using identity subject: %s, issuer: %s", identity.Subject, identity.Issuer)

	return provider, identity
}

// New creates a new writable statusmanager for testing using chainctl authentication.
// It automatically sets up the OIDC provider and extracts the signing identity.
// The identity parameter is the statusmanager identity string (e.g., "test-reconciler").
// Additional options can be passed (e.g., WithRepositoryOverride).
func New[T any](ctx context.Context, t *testing.T, identity string, opts ...statusmanager.Option) (*statusmanager.Manager[T], error) {
	t.Helper()

	provider, cosignIdentity := setupProviderAndIdentity(t, ctx)

	// Build options list: provider and identity first, then user-provided options
	allOpts := []statusmanager.Option{
		statusmanager.WithOIDCProvider(provider),
		statusmanager.WithExpectedIdentity(cosignIdentity),
	}
	allOpts = append(allOpts, opts...)

	return statusmanager.New[T](ctx, identity, allOpts...)
}

// NewReadOnly creates a new read-only statusmanager for testing using chainctl authentication.
// It automatically sets up the OIDC provider and extracts the signing identity.
// The identity parameter is the statusmanager identity string (e.g., "test-reconciler").
// Additional options can be passed (e.g., WithRepositoryOverride).
func NewReadOnly[T any](ctx context.Context, t *testing.T, identity string, opts ...statusmanager.Option) (*statusmanager.Manager[T], error) {
	t.Helper()

	provider, cosignIdentity := setupProviderAndIdentity(t, ctx)

	// Build options list: provider and identity first, then user-provided options
	allOpts := []statusmanager.Option{
		statusmanager.WithOIDCProvider(provider),
		statusmanager.WithExpectedIdentity(cosignIdentity),
	}
	allOpts = append(allOpts, opts...)

	return statusmanager.NewReadOnly[T](ctx, identity, allOpts...)
}
