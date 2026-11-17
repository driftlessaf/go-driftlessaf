/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsign

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/fulcio/pkg/certificate"
	siggit "github.com/sigstore/gitsign/pkg/git"
	"github.com/sigstore/gitsign/pkg/rekor"
	"github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
)

// GoogleIssuer is the OIDC issuer that Fulcio records in certificates issued
// to Google service accounts, such as the identity NewSigner uses on Cloud Run.
const GoogleIssuer = "https://accounts.google.com"

const (
	rekorURL              = "https://rekor.sigstore.dev"
	maxSignedCommit       = 1 << 20
	maxSignaturePositions = 32
	verifyTimeout         = 2 * time.Minute
)

// serviceAccount matches the only identities VerifyCommit accepts: an exact
// Google service-account email, never a pattern or a person.
var serviceAccount = regexp.MustCompile(`^[a-z0-9-]+@[a-z0-9-]+\.iam\.gserviceaccount\.com$`)

// The CMS BER decoder in smimesign v0.2.0 mutates package-level state.
var cmsMu sync.Mutex

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// InclusionFunc checks a signature's Rekor inclusion receipt for the signing
// certificate and returns the verified log entry.
type InclusionFunc func(ctx context.Context, sig []byte, cert *x509.Certificate) (*models.LogEntryAnon, error)

// VerifyCommit checks that the commit hash in repo carries a gitsign signature
// from identity, a Google service-account email issued by GoogleIssuer.
//
// The certificate must chain to the public-good Fulcio roots from Sigstore
// TUF, and the signature must embed a Rekor inclusion receipt whose
// integrated time falls inside the certificate's validity. Each call loads
// fresh trust roots; failing to load them fails verification. Verification
// has a two-minute deadline, and signed commit objects are limited to 1 MiB.
func VerifyCommit(ctx context.Context, repo *gogit.Repository, hash plumbing.Hash, identity string) error {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	data, err := signedCommit(repo, hash)
	if err != nil {
		return err
	}
	certs, err := siggit.NewDefaultVerifier(ctx)
	if err != nil {
		return fmt.Errorf("loading Fulcio trust roots: %w", err)
	}
	log, err := rekor.NewWithOptions(ctx, rekorURL)
	if err != nil {
		return fmt.Errorf("loading Rekor trust roots: %w", err)
	}
	return verifyCommitSignature(ctx, data, identity, certs, func(ctx context.Context, sig []byte, cert *x509.Certificate) (*models.LogEntryAnon, error) {
		cmsMu.Lock()
		defer cmsMu.Unlock()
		return log.VerifyInclusion(ctx, sig, cert)
	})
}

// Verifier checks commit signatures against its trust roots and Rekor
// inclusion check. NewDefaultVerifier uses the public-good roots, and
// NewVerifier lets tests substitute local roots and receipts.
type Verifier struct {
	certs     *siggit.CertVerifier
	inclusion InclusionFunc
}

// NewVerifier returns a Verifier that checks certificates with certs and
// Rekor receipts with inclusion. A nil argument makes every check fail.
func NewVerifier(certs *siggit.CertVerifier, inclusion InclusionFunc) *Verifier {
	return &Verifier{certs: certs, inclusion: inclusion}
}

// VerifyCommit applies the same checks as the package-level VerifyCommit,
// using v's trust roots and inclusion check, and without its deadline.
func (v *Verifier) VerifyCommit(ctx context.Context, repo *gogit.Repository, hash plumbing.Hash, identity string) error {
	data, err := signedCommit(repo, hash)
	if err != nil {
		return err
	}
	return verifyCommitSignature(ctx, data, identity, v.certs, v.inclusion)
}

// TrustRootError reports that Sigstore trust roots could not be loaded, so the
// signature was not checked. A caller may retry the check later.
type TrustRootError struct {
	Cause error
}

func (e *TrustRootError) Error() string {
	return fmt.Sprintf("loading Sigstore trust roots: %v", e.Cause)
}

func (e *TrustRootError) Unwrap() error {
	return e.Cause
}

// Identity is the OIDC issuer and subject of a signing certificate that Fulcio
// issued.
type Identity struct {
	Issuer  string
	Subject string
}

// SignedCommit is a commit as GitHub's commit verification reports it: the
// commit's ID, the commit object without its gpgsig header, and the signature
// from that header.
type SignedCommit struct {
	ID        string
	Payload   []byte
	Signature []byte
}

// IdentifyCommit returns the identity of the certificate that signed commit.
// The payload with the signature added back as its gpgsig header must hash to
// the commit's ID. The signature must verify over the payload with a
// certificate that chains to v's roots, and must embed a Rekor inclusion
// receipt whose integrated time falls inside the certificate's validity.
// The object may be at most 1 MiB, and the signature must be at one of the
// last 32 header boundaries.
func (v *Verifier) IdentifyCommit(ctx context.Context, commit SignedCommit) (Identity, error) {
	raw, err := commitObject(ctx, commit)
	if err != nil {
		return Identity{}, err
	}
	data, err := splitSignedCommit(raw)
	if err != nil {
		return Identity{}, err
	}
	if !bytes.Equal(data.Payload, commit.Payload) {
		return Identity{}, errors.New("reconstructed commit has a different signed payload")
	}
	cert, err := verifySignature(ctx, data, v.certs, v.inclusion)
	if err != nil {
		return Identity{}, err
	}
	return certificateIdentity(cert)
}

func certificateIdentity(cert *x509.Certificate) (Identity, error) {
	extensions, err := certificate.ParseExtensions(cert.Extensions)
	if err != nil {
		return Identity{}, fmt.Errorf("parsing certificate extensions: %w", err)
	}
	sans := cryptoutils.GetSubjectAlternateNames(cert)
	if extensions.Issuer == "" || len(sans) != 1 || sans[0] == "" {
		return Identity{}, errors.New("certificate requires an issuer and exactly one subject")
	}
	return Identity{Issuer: extensions.Issuer, Subject: sans[0]}, nil
}

func commitObject(ctx context.Context, commit SignedCommit) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !commitID.MatchString(commit.ID) {
		return nil, errors.New("commit ID must be a lowercase SHA-1")
	}
	id := plumbing.NewHash(commit.ID)
	signature := bytes.TrimSuffix(commit.Signature, []byte("\n"))
	if len(signature) == 0 || len(commit.Payload) > maxSignedCommit || len(signature) > maxSignedCommit-len(commit.Payload) {
		return nil, errors.New("commit requires a bounded gitsign signature")
	}
	end := bytes.Index(commit.Payload, []byte("\n\n"))
	if end < 0 {
		return nil, errors.New("commit payload has no message separator")
	}
	const prefix = "gpgsig "
	size := len(commit.Payload) + len(prefix) + len(signature) + bytes.Count(signature, []byte("\n")) + 1
	if size > maxSignedCommit {
		return nil, errors.New("signed commit must be at most 1 MiB")
	}
	header := append(append([]byte(prefix), bytes.ReplaceAll(signature, []byte("\n"), []byte("\n "))...), '\n')
	// GitHub omits the position of gpgsig. Git and go-git normally put it
	// last. Search backwards across complete headers for other encoders.
	attempts := 0
	for i := end; i >= 0; i = bytes.LastIndexByte(commit.Payload[:i], '\n') {
		next := i + 1
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if commit.Payload[next] == ' ' || commit.Payload[next] == '\t' {
			continue
		}
		if attempts == maxSignaturePositions {
			return nil, errors.New("too many possible signature positions")
		}
		attempts++
		hash := plumbing.NewHasher(plumbing.CommitObject, int64(size))
		hash.Write(commit.Payload[:next])
		hash.Write(header)
		hash.Write(commit.Payload[next:])
		if hash.Sum() == id {
			return slices.Concat(commit.Payload[:next], header, commit.Payload[next:]), nil
		}
	}
	return nil, errors.New("signed payload does not hash to the commit ID")
}

func signedCommit(repo *gogit.Repository, hash plumbing.Hash) (*siggit.CommitSig, error) {
	obj, err := repo.Storer.EncodedObject(plumbing.CommitObject, hash)
	if err != nil {
		return nil, fmt.Errorf("loading signed commit: %w", err)
	}
	if obj.Size() <= 0 || obj.Size() > maxSignedCommit {
		return nil, errors.New("signed commit must be at most 1 MiB")
	}
	r, err := obj.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, maxSignedCommit+1))
	if err != nil {
		return nil, err
	}
	return splitSignedCommit(raw)
}

// splitSignedCommit separates raw, an encoded commit object, into its signature
// and the signed payload.
func splitSignedCommit(raw []byte) (*siggit.CommitSig, error) {
	if len(raw) == 0 || len(raw) > maxSignedCommit {
		return nil, errors.New("signed commit must be at most 1 MiB")
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.CommitObject)
	if _, err := obj.Write(raw); err != nil {
		return nil, err
	}
	if err := siggit.ValidateCommit(obj); err != nil {
		return nil, fmt.Errorf("validating commit headers: %w", err)
	}
	headers, body, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok || len(raw) > maxSignedCommit || bytes.ContainsRune(headers, '\r') {
		return nil, errors.New("invalid signed commit headers or size")
	}
	// SplitCommit uses a line scanner, which would normalize message CRLFs and
	// add a final newline. Split only LF headers and preserve the body verbatim;
	// re-encoding a parsed Commit could also discard attacker-added headers.
	data, err := siggit.SplitCommit(bytes.NewReader(raw[:len(headers)+2]))
	if err != nil {
		return nil, fmt.Errorf("splitting signed commit: %w", err)
	}
	if len(data.Gpgsig) == 0 || len(data.Payload)+len(data.Gpgsig) > maxSignedCommit {
		return nil, errors.New("commit requires a bounded gitsign signature")
	}
	if len(data.GpgsigSha256) != 0 {
		return nil, errors.New("unexpected alternate object-format signature")
	}
	data.Payload = append(data.Payload, body...)
	return data, nil
}

func verifyCommitSignature(ctx context.Context, data *siggit.CommitSig, identity string, certs *siggit.CertVerifier, inclusion InclusionFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !serviceAccount.MatchString(identity) {
		return errors.New("expected signer and signature verification are required")
	}
	cert, err := verifyCertificate(ctx, data, certs, inclusion)
	if err != nil {
		return err
	}
	if err := cosign.CheckCertificatePolicy(cert, &cosign.CheckOpts{
		Identities: []cosign.Identity{{Issuer: GoogleIssuer, Subject: identity}},
	}); err != nil {
		return fmt.Errorf("verifying signer identity: %w", err)
	}
	if err := verifyInclusion(ctx, data, cert, inclusion); err != nil {
		return err
	}
	return ctx.Err()
}

// verifySignature checks data's signature and its Rekor inclusion receipt, and
// returns the signing certificate.
func verifySignature(ctx context.Context, data *siggit.CommitSig, certs *siggit.CertVerifier, inclusion InclusionFunc) (*x509.Certificate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cert, err := verifyCertificate(ctx, data, certs, inclusion)
	if err != nil {
		return nil, err
	}
	if err := verifyInclusion(ctx, data, cert, inclusion); err != nil {
		return nil, err
	}
	return cert, ctx.Err()
}

// verifyCertificate checks that data's signature verifies over its payload with
// a certificate that chains to certs, and returns that certificate.
func verifyCertificate(ctx context.Context, data *siggit.CommitSig, certs *siggit.CertVerifier, inclusion InclusionFunc) (*x509.Certificate, error) {
	if data == nil || certs == nil || inclusion == nil {
		return nil, errors.New("expected signer and signature verification are required")
	}
	cmsMu.Lock()
	cert, err := certs.Verify(ctx, data.Payload, data.Gpgsig, true)
	cmsMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("verifying commit signature: %w", err)
	}
	if cert == nil {
		return nil, errors.New("signature verification returned no certificate")
	}
	return cert, nil
}

// verifyInclusion checks the Rekor receipt in data's signature for cert, and
// that the entry's integrated time falls inside cert's validity.
func verifyInclusion(ctx context.Context, data *siggit.CommitSig, cert *x509.Certificate, inclusion InclusionFunc) error {
	// The current signer embeds a Rekor receipt in its CMS signature. Require
	// that receipt, including its cryptographic inclusion proof and SET, rather
	// than falling back to a legacy online search or accepting a bare signature.
	entry, err := inclusion(ctx, data.Gpgsig, cert)
	if err != nil {
		return fmt.Errorf("verifying Rekor inclusion: %w", err)
	}
	if entry == nil || entry.IntegratedTime == nil {
		return errors.New("verified Rekor entry has no integrated time")
	}
	when := time.Unix(*entry.IntegratedTime, 0)
	if when.Before(cert.NotBefore) || when.After(cert.NotAfter) {
		return errors.New("verified Rekor integrated time is outside the signing certificate's validity")
	}
	return nil
}
