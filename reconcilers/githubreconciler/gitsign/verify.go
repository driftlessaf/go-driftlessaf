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
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	siggit "github.com/sigstore/gitsign/pkg/git"
	"github.com/sigstore/gitsign/pkg/rekor"
	"github.com/sigstore/rekor/pkg/generated/models"
)

// GoogleIssuer is the OIDC issuer that Fulcio records in certificates issued
// to Google service accounts, such as the identity NewSigner uses on Cloud Run.
const GoogleIssuer = "https://accounts.google.com"

const (
	rekorURL        = "https://rekor.sigstore.dev"
	maxSignedCommit = 1 << 20
	verifyTimeout   = 2 * time.Minute
)

// serviceAccount matches the only identities VerifyCommit accepts: an exact
// Google service-account email, never a pattern or a person.
var serviceAccount = regexp.MustCompile(`^[a-z0-9-]+@[a-z0-9-]+\.iam\.gserviceaccount\.com$`)

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
	return verifyCommitSignature(ctx, data, identity, certs, log.VerifyInclusion)
}

// Verifier checks commit signatures against caller-supplied trust roots and
// Rekor inclusion check. Production code uses VerifyCommit; Verifier exists
// so tests can substitute local roots and receipts.
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

func signedCommit(repo *gogit.Repository, hash plumbing.Hash) (*siggit.CommitSig, error) {
	obj, err := repo.Storer.EncodedObject(plumbing.CommitObject, hash)
	if err != nil {
		return nil, fmt.Errorf("loading signed commit: %w", err)
	}
	if obj.Size() <= 0 || obj.Size() > maxSignedCommit {
		return nil, errors.New("signed commit must be at most 1 MiB")
	}
	if err := siggit.ValidateCommit(obj); err != nil {
		return nil, fmt.Errorf("validating commit headers: %w", err)
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
	if data == nil || !serviceAccount.MatchString(identity) || certs == nil || inclusion == nil {
		return errors.New("expected signer and signature verification are required")
	}
	cert, err := certs.Verify(ctx, data.Payload, data.Gpgsig, true)
	if err != nil {
		return fmt.Errorf("verifying commit signature: %w", err)
	}
	if cert == nil {
		return errors.New("signature verification returned no certificate")
	}
	if err := cosign.CheckCertificatePolicy(cert, &cosign.CheckOpts{
		Identities: []cosign.Identity{{Issuer: GoogleIssuer, Subject: identity}},
	}); err != nil {
		return fmt.Errorf("verifying signer identity: %w", err)
	}
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
	return ctx.Err()
}
