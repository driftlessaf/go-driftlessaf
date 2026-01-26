/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"fmt"

	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/fulcio"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/types"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/rekor/pkg/generated/client"
)

const (
	defaultFulcioURL = "https://fulcio.sigstore.dev"
	defaultRekorURL  = "https://rekor.sigstore.dev"
	defaultUserAgent = "ocireconciler-statusmanager"
)

// Option customizes the Manager.
type Option func(*config)

type config struct {
	fulcioURL        string
	rekorURL         string
	remoteOpts       []remote.Option
	repoOverride     *name.Repository
	signer           types.CosignerSignerVerifier
	rekor            *client.Rekor
	oidcProvider     fulcio.OIDCProvider
	userAgent        string
	expectedIdentity *cosign.Identity
}

func defaultConfig() *config {
	return &config{
		fulcioURL: defaultFulcioURL,
		rekorURL:  defaultRekorURL,
		userAgent: defaultUserAgent,
	}
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

// WithSigner injects a preconfigured signer (useful for tests).
func WithSigner(s types.CosignerSignerVerifier) Option {
	return func(c *config) { c.signer = s }
}

// WithOIDCProvider overrides the OIDC provider used for Fulcio keyless signing.
func WithOIDCProvider(p fulcio.OIDCProvider) Option {
	return func(c *config) { c.oidcProvider = p }
}

// WithUserAgent customizes the user-agent attached to Fulcio/Rekor requests.
func WithUserAgent(ua string) Option {
	return func(c *config) { c.userAgent = ua }
}

// WithExpectedIdentity specifies the sigstore identity to verify when reading
// attestations. This option is required for read-only managers and must not be
// provided for writable managers (which extract the identity from their credentials).
func WithExpectedIdentity(identity cosign.Identity) Option {
	return func(c *config) { c.expectedIdentity = &identity }
}
