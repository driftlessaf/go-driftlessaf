/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
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
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const sigstoreAudience = "sigstore"

// Predicated is implemented by status payload schemas to declare the in-toto
// predicate type their attestations carry. The predicate type, not the Go type,
// is what separates one Manager's attestations from another's on a shared
// subject: writes replace only referrers carrying a matching type, and reads
// accept only those.
//
// Implement it on a leaf schema with a value receiver returning a constant,
// and instantiate the Manager with the value type. A method on a type meant for
// embedding is promoted to every embedder, handing them the same predicate type
// by default.
type Predicated interface {
	PredicateType() string
}

// predicateTypeOf returns T's predicate type, rejecting values the write path
// cannot round-trip: secant rewrites cosign's aliases ("spdx", "custom") on
// write while reads match the literal, and a URI without a scheme is not a
// predicate type in-toto can carry.
func predicateTypeOf[T Predicated]() (string, error) {
	var zero T
	// A pointer or interface T zeroes to nil, and reaching a value-receiver
	// method through it panics, so reject the shape before the call.
	if v := reflect.ValueOf(zero); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return "", fmt.Errorf("%T cannot declare a predicate type: instantiate the Manager with the value type, not a pointer or interface", zero)
	}
	predicateType := zero.PredicateType()
	if predicateType == "" {
		return "", fmt.Errorf("%T declares an empty predicate type", zero)
	}
	if u, err := url.ParseRequestURI(predicateType); err != nil || u.Scheme == "" {
		return "", fmt.Errorf("%T declares predicate type %q: must be an absolute URI with a scheme", zero, predicateType)
	}
	return predicateType, nil
}

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
type Manager[T Predicated] struct {
	predicateType string
	readOnly      bool

	signer       *secant.BundleSigner
	verifier     *verify.Verifier
	certIdentity verify.CertificateIdentity

	remoteOpts   []remote.Option
	repoOverride *name.Repository
}

// Session represents reconciliation state for a single digest.
type Session[T Predicated] struct {
	manager *Manager[T]
	digest  name.Digest
	subject name.Digest
}

// New constructs a Manager capable of mutating attestations.
func New[T Predicated](ctx context.Context, opts ...Option) (*Manager[T], error) {
	return newManager[T](ctx, false, opts...)
}

// NewReadOnly constructs a Manager that can only read status.
func NewReadOnly[T Predicated](ctx context.Context, opts ...Option) (*Manager[T], error) {
	return newManager[T](ctx, true, opts...)
}

func newManager[T Predicated](ctx context.Context, readOnly bool, opts ...Option) (*Manager[T], error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	// Before any network work, so a bad predicate type fails fast.
	predicateType, err := predicateTypeOf[T]()
	if err != nil {
		return nil, err
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

	// The verifier and certificate identity are immutable for the life of
	// the Manager, so build them once here instead of per read. The option
	// set mirrors what cosign.VerifyNewBundle derives for these settings
	// (certificate-based verification with SCTs, a transparency log entry,
	// and a signed timestamp, each at threshold 1). The statement predicate
	// is omitted from verification results because verifyAndExtract decodes
	// it directly from the DSSE payload bytes; materializing the predicate
	// in the result would repeat that work through protojson at several
	// times the allocation cost.
	sanMatcher, err := verify.NewSANMatcher(signingIdentity.Subject, signingIdentity.SubjectRegExp)
	if err != nil {
		return nil, fmt.Errorf("creating SAN matcher: %w", err)
	}
	issuerMatcher, err := verify.NewIssuerMatcher(signingIdentity.Issuer, signingIdentity.IssuerRegExp)
	if err != nil {
		return nil, fmt.Errorf("creating issuer matcher: %w", err)
	}
	certIdentity, err := verify.NewCertificateIdentity(sanMatcher, issuerMatcher, certificate.Extensions{})
	if err != nil {
		return nil, fmt.Errorf("creating certificate identity: %w", err)
	}
	// WithoutStatementPredicate would skip materializing the predicate as a
	// structpb tree (see verifyAndExtract), but it only exists in sigstore-go
	// v1.3.0+, and sigstore-go is held at v1.2.2 until gitsign moves off it —
	// gitsign's validatingRekorClient does not satisfy the v1.3.0 sign.RekorClient
	// interface. Restore the option once sigstore/gitsign#861 lands and the pin
	// in this module's go.mod goes away.
	verifier, err := verify.NewVerifier(trustedMaterial,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithSignedTimestamps(1),
	)
	if err != nil {
		return nil, fmt.Errorf("creating bundle verifier: %w", err)
	}

	remoteOpts := slices.Clone(cfg.remoteOpts)
	if cfg.repoOverride != nil {
		// A repository override pins every bundle read and write to a
		// single repository under the manager's process-stable identity,
		// so the token exchanges and per-repo state that ggcr's
		// Puller/Pusher cache are safe to share for the life of the
		// Manager — collapsing the per-operation /v2/token exchange (and
		// registry ping) to once per process. Without an override,
		// subjects live in arbitrary repositories whose credentials may
		// be request-scoped (e.g. per-tenant impersonation), so each call
		// keeps its clean-slate options.
		puller, err := remote.NewPuller(remoteOpts...)
		if err != nil {
			return nil, fmt.Errorf("creating shared puller: %w", err)
		}
		remoteOpts = append(remoteOpts, remote.Reuse(puller))
		if !readOnly {
			pusher, err := remote.NewPusher(remoteOpts...)
			if err != nil {
				return nil, fmt.Errorf("creating shared pusher: %w", err)
			}
			remoteOpts = append(remoteOpts, remote.Reuse(pusher))
		}
	}

	return &Manager[T]{
		predicateType: predicateType,
		readOnly:      readOnly,
		signer:        signer,
		verifier:      verifier,
		certIdentity:  certIdentity,
		remoteOpts:    remoteOpts,
		repoOverride:  cfg.repoOverride,
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
	return s.manager.fetchLatestStatus(ctx, s.subject, s.manager.predicateType)
}

// ObservedStateWithOptions returns the latest recorded status using call-local
// options that do not mutate the Manager or Session.
func (s *Session[T]) ObservedStateWithOptions(ctx context.Context, opts ...CallOption) (*Status[T], error) {
	predicateType, err := s.observedPredicateType(opts...)
	if err != nil {
		return nil, err
	}
	return s.manager.fetchLatestStatus(ctx, s.subject, predicateType)
}

func (s *Session[T]) observedPredicateType(opts ...CallOption) (string, error) {
	cfg := callConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.predicateType != "" {
		if u, err := url.ParseRequestURI(cfg.predicateType); err != nil || u.Scheme == "" {
			return "", fmt.Errorf("predicate type %q must be an absolute URI with a scheme", cfg.predicateType)
		}
	}
	return cmp.Or(cfg.predicateType, s.manager.predicateType), nil
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
	// SkipSame short-circuits before signing when an existing bundle carries a
	// byte-identical payload, so re-persisting an unchanged status costs a
	// referrer read instead of a Fulcio certificate, a Rekor upload, and a
	// referrer write. When the payload differs, superseded bundles are deleted
	// before the replacement is written, scoped to this Manager's predicate
	// type, so Managers over different schemas can share a subject without
	// reaping each other's attestations. A reader racing the swap still verifies
	// whichever bundles remain visible and picks the latest (see
	// fetchLatestStatus). Registry retention cannot be relied on here:
	// Artifact Registry cleanup policies operate on versions, and referrer
	// manifests are not versions.
	if err := transient.Retry(ctx, "writing attestation bundle", retryable, func(ctx context.Context) error {
		return secant.AttestBundle(ctx, secant.SkipSame, []*types.Statement{stmt}, s.manager.signer, s.manager.remoteOptions(ctx))
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

func (m *Manager[T]) fetchLatestStatus(ctx context.Context, subject name.Digest, predicateType string) (*Status[T], error) {
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

	policy := verify.NewPolicy(
		verify.WithArtifactDigest(subjectHash.Algorithm, subjectHashBytes),
		verify.WithCertificateIdentity(m.certIdentity))

	var latest *statusCandidate[T]
	for _, b := range bundles {
		status, ts, ok := m.verifyAndExtract(ctx, b, policy, predicateType)
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
// in-toto statement predicate type doesn't match predicateType, returning the
// parsed Status[T] alongside the verified timestamp.
//
// The predicate is decoded directly from the DSSE payload bytes — the exact
// bytes the verified signature covers — rather than from the verification
// result's Statement. The Statement's predicate is a structpb tree, and
// round-tripping a large findings payload through protojson.Marshal and a
// second json.Unmarshal multiplies its size in allocations several times
// over; on findings-heavy statuses that churn was the dominant memory cost
// of a status read. Only the statement type, subjects, and predicate type are
// read from the result, alongside the verified timestamp, so decoding the
// payload directly avoids that cost regardless of whether the verifier
// materialized a predicate.
func (m *Manager[T]) verifyAndExtract(ctx context.Context, b *sgbundle.Bundle, policy verify.PolicyBuilder, predicateType string) (*Status[T], time.Time, bool) {
	result, err := m.verifier.Verify(b, policy)
	if err != nil {
		clog.WarnContextf(ctx, "Bundle verification failed: %v", err)
		return nil, time.Time{}, false
	}
	if result.Statement == nil || result.Statement.PredicateType != predicateType {
		return nil, time.Time{}, false
	}

	payload, ok := dssePayload(b)
	if !ok {
		clog.WarnContext(ctx, "Skipping bundle without a DSSE payload")
		return nil, time.Time{}, false
	}
	var stmt struct {
		Predicate Status[T] `json:"predicate"`
	}
	if err := json.Unmarshal(payload, &stmt); err != nil {
		clog.WarnContextf(ctx, "Skipping bundle with unparseable status predicate: %v", err)
		return nil, time.Time{}, false
	}

	return &stmt.Predicate, bundleTimestamp(result), true
}

// dssePayload returns the raw in-toto statement JSON carried by the
// bundle's DSSE envelope. It reads the protobuf content directly:
// sgbundle's Envelope() accessor re-encodes the payload through base64,
// which is pure allocation overhead at this call site.
func dssePayload(b *sgbundle.Bundle) ([]byte, bool) {
	env, ok := b.Content.(*protobundle.Bundle_DsseEnvelope)
	if !ok || env.DsseEnvelope == nil {
		return nil, false
	}
	return env.DsseEnvelope.GetPayload(), true
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
	if _, ok := errors.AsType[*cosign.ErrImageTagNotFound](err); ok {
		return true
	}
	var noAtts *cosign.ErrNoMatchingAttestations
	return errors.As(err, &noAtts)
}
