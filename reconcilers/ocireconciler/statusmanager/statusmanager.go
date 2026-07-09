/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/transient"
	"chainguard.dev/sdk/auth"
	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/types"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	crtypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"
)

const sigstoreAudience = "sigstore"

// transientRekorErrors are the Rekor failure modes known to be transient.
// The rekor-tiles client returns untyped errors, flattening the HTTP status
// into the message ("unexpected response: <code> <body>"), so string
// matching is the only way to recognize them.
var transientRekorErrors = []string{
	"adding rekor v2 entry: unexpected response: 499",
	"adding rekor v2 entry: unexpected response: 502",
	"adding rekor v2 entry: unexpected response: 503 upstream connect error",
}

// Status captures serialized reconciliation progress for a digest.
type Status[T any] struct {
	ObservedGeneration string `json:"observedGeneration"`
	Details            T      `json:"details"`
}

// Manager writes and reads reconciliation status as attestations.
type Manager[T any] struct {
	identity        string
	signingIdentity cosign.Identity
	predicateType   string
	readOnly        bool

	signer          *secant.BundleSigner
	trustedMaterial root.TrustedMaterial

	remoteOpts   []remote.Option
	repoOverride *name.Repository
}

// Session represents reconciliation state for a single digest.
type Session[T any] struct {
	manager *Manager[T]
	digest  name.Digest
	subject name.Digest
}

// New constructs a Manager capable of mutating attestations.
func New[T any](ctx context.Context, identity string, opts ...Option) (*Manager[T], error) {
	return newManager[T](ctx, identity, false, opts...)
}

// NewReadOnly constructs a Manager that can only read status.
func NewReadOnly[T any](ctx context.Context, identity string, opts ...Option) (*Manager[T], error) {
	return newManager[T](ctx, identity, true, opts...)
}

func newManager[T any](ctx context.Context, identity string, readOnly bool, opts ...Option) (*Manager[T], error) {
	if strings.TrimSpace(identity) == "" {
		return nil, errors.New("identity is required")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.oidcProvider == nil && !readOnly {
		p, err := newGSAOIDCProvider(ctx, sigstoreAudience)
		if err != nil {
			return nil, fmt.Errorf("creating OIDC provider: %w", err)
		}
		cfg.oidcProvider = p
	}

	trustedMaterial := cfg.trustedMaterial
	if trustedMaterial == nil {
		tr, err := cosign.TrustedRoot()
		if err != nil {
			return nil, fmt.Errorf("loading trusted root from TUF: %w", err)
		}
		trustedMaterial = tr
	}

	var signer *secant.BundleSigner
	if !readOnly {
		if cfg.signer != nil {
			signer = cfg.signer
		} else {
			signingConfig := cfg.signingConfig
			if signingConfig == nil {
				sc, err := cosign.SigningConfigRekorV2()
				if err != nil {
					return nil, fmt.Errorf("loading Rekor v2 signing config: %w", err)
				}
				signingConfig = sc
			}
			bs, err := secant.NewBundleSigner(cfg.oidcProvider,
				secant.WithSigningConfig(signingConfig),
				secant.WithTrustedMaterial(trustedMaterial),
			)
			if err != nil {
				return nil, fmt.Errorf("creating bundle signer: %w", err)
			}
			signer = bs
		}
	}

	// Determine the signing identity to use for verification.
	var signingIdentity cosign.Identity
	switch {
	case cfg.expectedIdentity != nil:
		// Use explicitly provided identity for verification
		signingIdentity = *cfg.expectedIdentity
	case readOnly:
		// Read-only managers require an explicit identity
		return nil, errors.New("WithExpectedIdentity is required for read-only managers")
	default:
		// For writable managers without explicit identity, try to extract from token
		// Extract the signing identity from an ID token so we know what
		// identity to expect when verifying attestations. The audience doesn't
		// matter here, we just need any token to extract the identity.
		tok, err := cfg.oidcProvider.Provide(ctx, "garbage")
		if err != nil {
			return nil, fmt.Errorf("getting ID token to extract signing identity: %w", err)
		}
		subject, _, err := auth.ExtractEmail(tok)
		if err != nil {
			return nil, fmt.Errorf("extracting subject from token: %w", err)
		}
		issuer, err := auth.ExtractIssuer(tok)
		if err != nil {
			return nil, fmt.Errorf("extracting issuer from token: %w", err)
		}
		signingIdentity = cosign.Identity{
			Subject: subject,
			Issuer:  issuer,
		}
	}

	predicateType := fmt.Sprintf("https://statusmanager.chainguard.dev/%s", identity)

	return &Manager[T]{
		identity:        identity,
		signingIdentity: signingIdentity,
		predicateType:   predicateType,
		readOnly:        readOnly,
		signer:          signer,
		trustedMaterial: trustedMaterial,
		remoteOpts:      slices.Clone(cfg.remoteOpts),
		repoOverride:    cfg.repoOverride,
	}, nil
}

// NewSession initializes a reconciliation session for the provided digest.
func (m *Manager[T]) NewSession(digest name.Digest) *Session[T] {
	return &Session[T]{
		manager: m,
		digest:  digest,
		subject: m.subjectDigest(digest),
	}
}

// ObservedState returns the latest recorded status, if any.
func (s *Session[T]) ObservedState(ctx context.Context) (*Status[T], error) {
	return s.manager.fetchLatestStatus(ctx, s.subject)
}

// SetActualState persists the provided status as an attestation. Transient
// write failures are retried in-process; if they persist, the returned error
// satisfies transient.Is.
func (s *Session[T]) SetActualState(ctx context.Context, status *Status[T]) error {
	if s.manager.readOnly {
		return errors.New("status manager is read-only")
	}
	if status == nil {
		return errors.New("status cannot be nil")
	}
	status.ObservedGeneration = s.subject.DigestStr()

	payload, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshaling status: %w", err)
	}

	stmt, err := secant.NewStatement(s.subject, bytes.NewReader(payload), s.manager.predicateType)
	if err != nil {
		return fmt.Errorf("creating statement: %w", err)
	}

	// Status subjects are synthetic digests that may exist in no registry, so
	// supply the subject descriptor rather than letting AttestBundle resolve it
	// via HEAD.
	h, err := v1.NewHash(s.subject.DigestStr())
	if err != nil {
		return fmt.Errorf("parsing subject digest %q: %w", s.subject.DigestStr(), err)
	}
	stmt.SubjectDescriptor = &v1.Descriptor{
		MediaType: crtypes.OCIManifestSchema1,
		Digest:    h,
	}

	// Retry temporary registry errors and, since Rekor errors are untyped,
	// the Rekor failure modes known to be transient.
	retryable := func(err error) bool {
		if transient.Is(err) {
			return true
		}
		msg := err.Error()
		for _, s := range transientRekorErrors {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
	if err := transient.Retry(ctx, "writing attestation bundle", retryable, func(ctx context.Context) error {
		return secant.AttestBundle(ctx, secant.Replace, []*types.Statement{stmt}, s.manager.signer, s.manager.remoteOptions(ctx))
	}); err != nil {
		return fmt.Errorf("writing attestation bundle: %w", err)
	}
	return nil
}

func (m *Manager[T]) subjectDigest(d name.Digest) name.Digest {
	if m.repoOverride == nil {
		return d
	}
	return m.repoOverride.Digest(d.DigestStr())
}

func (m *Manager[T]) fetchLatestStatus(ctx context.Context, subject name.Digest) (*Status[T], error) {
	bundles, subjectHash, err := cosign.GetBundles(ctx, subject, m.ociremoteOptions(ctx))
	if err != nil {
		if isMissing(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching bundles: %w", err)
	}

	subjectHashBytes, err := hex.DecodeString(subjectHash.Hex)
	if err != nil {
		return nil, fmt.Errorf("decoding subject digest hex: %w", err)
	}

	checkOpts := m.newCheckOpts(ctx)
	policyOpt := verify.WithArtifactDigest(subjectHash.Algorithm, subjectHashBytes)

	var latest *statusCandidate[T]
	for _, b := range bundles {
		status, ts, ok := m.verifyAndExtract(ctx, b, checkOpts, policyOpt)
		if !ok {
			continue
		}
		if latest == nil || ts.After(latest.timestamp) {
			latest = &statusCandidate[T]{status: status, timestamp: ts}
		}
	}
	if latest == nil {
		return nil, nil
	}
	return latest.status, nil
}

// verifyAndExtract verifies the bundle, then filters out any whose verified
// in-toto statement predicate type doesn't match this manager's, returning the
// parsed Status[T] alongside the verified timestamp. VerifyNewBundle already
// decodes the DSSE envelope and returns the verified statement, so we read the
// predicate from there rather than re-parsing the envelope ourselves.
func (m *Manager[T]) verifyAndExtract(ctx context.Context, b *sgbundle.Bundle, co *cosign.CheckOpts, policyOpt verify.ArtifactPolicyOption) (*Status[T], time.Time, bool) {
	result, err := cosign.VerifyNewBundle(ctx, co, policyOpt, b)
	if err != nil {
		clog.WarnContextf(ctx, "Bundle verification failed: %v", err)
		return nil, time.Time{}, false
	}
	if result.Statement == nil || result.Statement.PredicateType != m.predicateType {
		return nil, time.Time{}, false
	}

	predicateBytes, err := protojson.Marshal(result.Statement.Predicate)
	if err != nil {
		clog.WarnContextf(ctx, "Skipping bundle with unmarshalable predicate: %v", err)
		return nil, time.Time{}, false
	}
	var status Status[T]
	if err := json.Unmarshal(predicateBytes, &status); err != nil {
		clog.WarnContextf(ctx, "Skipping bundle with unparseable status predicate: %v", err)
		return nil, time.Time{}, false
	}

	return &status, bundleTimestamp(result), true
}

func (m *Manager[T]) newCheckOpts(ctx context.Context) *cosign.CheckOpts {
	return &cosign.CheckOpts{
		RegistryClientOpts:  m.ociremoteOptions(ctx),
		Identities:          []cosign.Identity{m.signingIdentity},
		TrustedMaterial:     m.trustedMaterial,
		NewBundleFormat:     true,
		UseSignedTimestamps: true,
	}
}

type statusCandidate[T any] struct {
	status    *Status[T]
	timestamp time.Time
}

func bundleTimestamp(result *verify.VerificationResult) time.Time {
	if result == nil {
		return time.Time{}
	}
	var latest time.Time
	for _, t := range result.VerifiedTimestamps {
		if t.Timestamp.After(latest) {
			latest = t.Timestamp
		}
	}
	return latest
}

func (m *Manager[T]) remoteOptions(ctx context.Context) []remote.Option {
	return append([]remote.Option{remote.WithContext(ctx)}, m.remoteOpts...)
}

func (m *Manager[T]) ociremoteOptions(ctx context.Context) []ociremote.Option {
	opts := []ociremote.Option{ociremote.WithRemoteOptions(m.remoteOptions(ctx)...)}
	if m.repoOverride != nil {
		opts = append(opts, ociremote.WithTargetRepository(*m.repoOverride))
	}
	return opts
}

// isMissing returns true for the errors that indicate "no status attestation
// has been written yet": a 404 on the subject lookup, or no matching bundle
// referrers.
func isMissing(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		return true
	}
	var tagNotFound *cosign.ErrImageTagNotFound
	if errors.As(err, &tagNotFound) {
		return true
	}
	var noAtts *cosign.ErrNoMatchingAttestations
	return errors.As(err, &noAtts)
}
