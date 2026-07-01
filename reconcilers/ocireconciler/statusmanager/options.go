/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"fmt"

	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/fulcio"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// Option customizes the Manager.
type Option func(*config)

type config struct {
	remoteOpts       []remote.Option
	repoOverride     *name.Repository
	signer           *secant.BundleSigner
	signingConfig    *root.SigningConfig
	trustedMaterial  root.TrustedMaterial
	oidcProvider     fulcio.OIDCProvider
	expectedIdentity *cosign.Identity
}

func defaultConfig() *config {
	return &config{}
}

// WithRemoteOptions appends remote.Options applied when reading/writing attestations.
func WithRemoteOptions(opts ...remote.Option) Option {
	return func(c *config) { c.remoteOpts = append(c.remoteOpts, opts...) }
}

// WithRepositoryOverride directs attestation writes to the provided repository string.
func WithRepositoryOverride(repo string) Option {
	return func(c *config) {
		if repo == "" {
			c.repoOverride = nil
			return
		}
		r, err := name.NewRepository(repo)
		if err != nil {
			panic(fmt.Sprintf("invalid repository override %q: %v", repo, err))
		}
		c.repoOverride = &r
	}
}

// WithBundleSigner injects a preconfigured bundle signer (useful for tests).
// When provided, the manager skips its default signer construction.
func WithBundleSigner(bs *secant.BundleSigner) Option {
	return func(c *config) { c.signer = bs }
}

// WithSigningConfig overrides the SigningConfig used to construct the default
// bundle signer. Ignored if WithBundleSigner is also provided. Use this to
// point at a different Fulcio/Rekor topology than the embedded Rekor v2 config.
func WithSigningConfig(sc *root.SigningConfig) Option {
	return func(c *config) { c.signingConfig = sc }
}

// WithTrustedMaterial overrides the TrustedRoot used for both signing and
// verification. By default the manager loads the trusted root from the public
// Sigstore TUF mirror via cosign.TrustedRoot().
func WithTrustedMaterial(tm root.TrustedMaterial) Option {
	return func(c *config) { c.trustedMaterial = tm }
}

// WithOIDCProvider overrides the OIDC provider used for Fulcio keyless signing.
func WithOIDCProvider(p fulcio.OIDCProvider) Option {
	return func(c *config) { c.oidcProvider = p }
}

// WithExpectedIdentity specifies the sigstore identity to verify when reading
// attestations. This option is required for read-only managers and must not be
// provided for writable managers (which extract the identity from their credentials).
func WithExpectedIdentity(identity cosign.Identity) Option {
	return func(c *config) { c.expectedIdentity = &identity }
}
