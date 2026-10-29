/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsigntest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign"
	cms "github.com/github/smimesign/ietf-cms"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sigstore/fulcio/pkg/certificate"
	siggit "github.com/sigstore/gitsign/pkg/git"
	"github.com/sigstore/rekor/pkg/generated/models"
)

var _ gogit.Signer = (*Fixture)(nil)

// Fixture is a signing authority with one leaf certificate. Tests may change
// its exported fields between signing and verifying to simulate failures.
type Fixture struct {
	key *ecdsa.PrivateKey

	// Cert is the leaf certificate that Sign uses.
	Cert *x509.Certificate
	// Certs holds the trust roots that VerifyCommit uses. Replace it to
	// simulate a signature from an untrusted chain.
	Certs *siggit.CertVerifier
	// Receipts maps each signature Sign produced to its Rekor entry. Delete
	// or edit entries to simulate missing or malformed receipts.
	Receipts map[string]*models.LogEntryAnon
	// ReceiptError, when set, makes every inclusion check fail with it.
	ReceiptError error
	// Now is the integrated time Sign records for new receipts.
	Now time.Time
}

// New returns a Fixture whose leaf certificate names identity as its email
// SAN and issuer as its Fulcio OIDC issuer. The leaf is valid for an hour
// either side of the current time.
func New(t testing.TB, identity, issuer string) *Fixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fixture root"},
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour)}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	extensions, err := (certificate.Extensions{Issuer: issuer}).Render()
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Fixture signer"},
		EmailAddresses: []string{identity}, ExtraExtensions: extensions, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	certs, err := siggit.NewCertVerifier(siggit.WithRootPool(roots))
	if err != nil {
		t.Fatal(err)
	}
	return &Fixture{key: key, Cert: leaf, Certs: certs, Receipts: make(map[string]*models.LogEntryAnon), Now: now}
}

// Sign returns a detached CMS signature over r's contents and records a
// receipt for it at f.Now.
func (f *Fixture) Sign(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	signed, err := cms.NewSignedData(data)
	if err != nil {
		return nil, err
	}
	if err := signed.Sign([]*x509.Certificate{f.Cert}, f.key); err != nil {
		return nil, err
	}
	signed.Detached()
	der, err := signed.ToDER()
	if err != nil {
		return nil, err
	}
	sig := pem.EncodeToMemory(&pem.Block{Type: "SIGNED MESSAGE", Bytes: der})
	f.Receipts[string(sig)] = &models.LogEntryAnon{IntegratedTime: new(f.Now.Unix())}
	return sig, nil
}

// Inclusion is a [gitsign.InclusionFunc] that accepts a signature only if
// Sign produced it with f.Cert.
func (f *Fixture) Inclusion(_ context.Context, sig []byte, cert *x509.Certificate) (*models.LogEntryAnon, error) {
	if f.ReceiptError != nil {
		return nil, f.ReceiptError
	}
	if !cert.Equal(f.Cert) {
		return nil, errors.New("receipt certificate mismatch")
	}
	entry, ok := f.Receipts[string(sig)]
	if !ok {
		return nil, errors.New("signature not logged")
	}
	return entry, nil
}

// VerifyCommit checks hash in repo as [gitsign.VerifyCommit] does, but
// against f.Certs and f.Inclusion. Its signature matches gitsign.VerifyCommit
// so tests can pass it wherever production code takes that function.
func (f *Fixture) VerifyCommit(ctx context.Context, repo *gogit.Repository, hash plumbing.Hash, identity string) error {
	return gitsign.NewVerifier(f.Certs, f.Inclusion).VerifyCommit(ctx, repo, hash, identity)
}
