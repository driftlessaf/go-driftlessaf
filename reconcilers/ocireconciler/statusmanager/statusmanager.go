/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"chainguard.dev/sdk/auth"
	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/fulcio"
	rclient "github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/rekor/client"
	"github.com/chainguard-dev/terraform-provider-cosign/pkg/private/secant/types"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/in-toto/in-toto-golang/in_toto"
	"github.com/secure-systems-lab/go-securesystemslib/dsse"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/cosign/v2/pkg/oci"
	ociremote "github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/fulcio/pkg/api"
	"github.com/sigstore/rekor/pkg/generated/client"
	"github.com/sigstore/sigstore/pkg/fulcioroots"
)

const (
	sigstoreAudience = "sigstore"

	// RekorHTTPLimit is the maximum HTTP request size accepted by Rekor's reverse proxy.
	//
	// This limit was determined empirically (2025-12-29) by generating realistic SBOM-like
	// payloads at varying sizes and measuring the actual HTTP request sizes after base64
	// encoding and DSSE envelope wrapping. Testing showed:
	//   - 75 MB payload → 127.6 MB HTTP request ✅ SUCCESS
	//   - 100 MB payload → 170.3 MB HTTP request ❌ FAILED (502 Bad Gateway)
	//
	// The limit (~150 MB) is imposed by Rekor's reverse proxy (nginx/load balancer),
	// not the Rekor application itself.
	//
	// For production use with airflow APK (14.3 MB SBOM, 63 vuln matches):
	//   - BEFORE metadata stripping: 116.52 MB status → 198 MB HTTP ❌ 502 Bad Gateway
	//   - AFTER metadata stripping: 13.95 MB status → 23.72 MB HTTP ✅ 201 Created
	RekorHTTPLimit = 150 * 1024 * 1024 // 150 MB

	// StatusJSONSizeLimit is the maximum serialized JSON status size before base64/DSSE overhead.
	//
	// Calculation: RekorHTTPLimit / 1.7 (empirically measured overhead factor)
	//   - Base64 encoding adds ~33% overhead (4/3 ratio)
	//   - DSSE envelope wrapping adds additional ~28% overhead
	//   - Combined overhead factor: ~1.7x
	//
	// This gives us: 150 MB / 1.7 ≈ 88 MB for the raw JSON status
	StatusJSONSizeLimit = RekorHTTPLimit * 10 / 17 // ~88 MB
)

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

	signer types.CosignerSignerVerifier
	rekor  *client.Rekor

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
	if cfg.oidcProvider == nil {
		p, err := newGSAOIDCProvider(ctx, sigstoreAudience)
		if err != nil {
			return nil, fmt.Errorf("creating OIDC provider: %w", err)
		}
		cfg.oidcProvider = p
	}
	if cfg.signer == nil {
		furl, err := url.Parse(cfg.fulcioURL)
		if err != nil {
			return nil, fmt.Errorf("parsing fulcio url: %w", err)
		}
		client := api.NewClient(furl, api.WithUserAgent(cfg.userAgent))
		signer, err := fulcio.NewSigner(cfg.oidcProvider, client)
		if err != nil {
			return nil, fmt.Errorf("creating fulcio signer: %w", err)
		}
		cfg.signer = signer
	}
	if cfg.rekor == nil {
		rekorClient, err := rclient.GetRekorClient(cfg.rekorURL, rclient.WithUserAgent(cfg.userAgent))
		if err != nil {
			return nil, fmt.Errorf("creating rekor client: %w", err)
		}
		cfg.rekor = rekorClient
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
		signer:          cfg.signer,
		rekor:           cfg.rekor,
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

// SetActualState persists the provided status as an attestation.
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

	// Check if the serialized status exceeds Rekor's limit
	payloadSize := len(payload)
	if payloadSize > StatusJSONSizeLimit {
		return fmt.Errorf("status size %.2f MB exceeds limit of %.2f MB",
			float64(payloadSize)/(1024*1024),
			float64(StatusJSONSizeLimit)/(1024*1024))
	}

	stmt, err := secant.NewStatement(s.subject, bytes.NewReader(payload), s.manager.predicateType)
	if err != nil {
		return fmt.Errorf("creating statement: %w", err)
	}

	if err := secant.Attest(ctx, secant.Replace, []*types.Statement{stmt}, s.manager.signer, s.manager.rekor, s.manager.remoteOptions(ctx)); err != nil {
		return fmt.Errorf("writing attestation: %w", err)
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
	// Compute the attestation tag directly rather than going through SignedEntity,
	// which requires the subject image to exist. This allows COSIGN_REPOSITORY-style
	// workflows where attestations are stored separately from the subject.
	attTag, err := ociremote.AttestationTag(subject, m.ociremoteOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("computing attestation tag: %w", err)
	}
	// Fetch attestations directly from the computed tag.
	attestations, err := ociremote.Signatures(attTag, m.ociremoteOptions(ctx)...)
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching attestations: %w", err)
	}

	// Create CheckOpts for verification.
	co, err := m.createCheckOpts(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating check opts: %w", err)
	}

	// Parse the subject digest for verification.
	h, err := v1.NewHash(subject.DigestStr())
	if err != nil {
		return nil, fmt.Errorf("parsing subject hash: %w", err)
	}

	// Verify attestations using cosign.
	verifiedAtts, bundleVerified, err := cosign.VerifyImageAttestation(ctx, attestations, h, co)
	if err != nil {
		// If verification fails entirely, treat as no attestations.
		clog.FromContext(ctx).Warnf("Attestation verification failed: %v", err)
		return nil, nil
	}
	if !bundleVerified {
		clog.FromContext(ctx).Warn("Attestation bundle not verified")
		return nil, nil
	}

	var latest *statusCandidate[T]
	for _, att := range verifiedAtts {
		ann, err := att.Annotations()
		if err != nil {
			clog.FromContext(ctx).Warnf("Skipping attestation: %v", err)
			continue
		}
		pt, ok := ann["predicateType"]
		if !ok {
			clog.FromContext(ctx).Warn("Skipping attestation without predicateType annotation")
			continue
		}
		if pt != m.predicateType {
			continue
		}
		status, err := extractStatus[T](att)
		if err != nil {
			clog.FromContext(ctx).Warnf("Failed to parse status attestation: %v", err)
			continue
		}
		candidate := &statusCandidate[T]{status: status, timestamp: integratedTime(att)}
		if latest == nil || candidate.timestamp.After(latest.timestamp) {
			latest = candidate
		}
	}
	if latest == nil {
		return nil, nil
	}
	return latest.status, nil
}

func (m *Manager[T]) createCheckOpts(ctx context.Context) (*cosign.CheckOpts, error) {
	fulcioRoots, err := fulcioroots.Get()
	if err != nil {
		return nil, fmt.Errorf("getting Fulcio roots: %w", err)
	}
	fulcioIntermediates, err := fulcioroots.GetIntermediates()
	if err != nil {
		return nil, fmt.Errorf("getting Fulcio intermediates: %w", err)
	}
	ctlogKeys, err := cosign.GetCTLogPubs(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting CTLog public keys: %w", err)
	}
	rekorPubKeys, err := cosign.GetRekorPubs(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting Rekor public keys: %w", err)
	}

	return &cosign.CheckOpts{
		RegistryClientOpts: m.ociremoteOptions(ctx),
		ClaimVerifier:      cosign.IntotoSubjectClaimVerifier,
		Identities:         []cosign.Identity{m.signingIdentity},
		RootCerts:          fulcioRoots,
		IntermediateCerts:  fulcioIntermediates,
		CTLogPubKeys:       ctlogKeys,
		RekorClient:        m.rekor,
		RekorPubKeys:       rekorPubKeys,
	}, nil
}

type statusCandidate[T any] struct {
	status    *Status[T]
	timestamp time.Time
}

func integratedTime(sig oci.Signature) time.Time {
	bundle, err := sig.Bundle()
	if err != nil || bundle == nil {
		return time.Time{}
	}
	if bundle.Payload.IntegratedTime == 0 {
		return time.Time{}
	}
	return time.Unix(bundle.Payload.IntegratedTime, 0)
}

func extractStatus[T any](sig oci.Signature) (*Status[T], error) {
	payload, err := sig.Payload()
	if err != nil {
		return nil, fmt.Errorf("reading payload: %w", err)
	}
	var env dsse.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("unmarshaling envelope: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("decoding payload: %w", err)
	}
	var stmt in_toto.Statement
	if err := json.Unmarshal(decoded, &stmt); err != nil {
		return nil, fmt.Errorf("unmarshaling statement: %w", err)
	}
	predicateBytes, err := json.Marshal(stmt.Predicate)
	if err != nil {
		return nil, fmt.Errorf("marshaling predicate: %w", err)
	}
	var status Status[T]
	if err := json.Unmarshal(predicateBytes, &status); err != nil {
		return nil, fmt.Errorf("unmarshaling status predicate: %w", err)
	}
	return &status, nil
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

// notFound returns true if err indicates a 404 response from the registry.
func notFound(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.StatusCode == 404
	}
	return false
}
