/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsign

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sigstore/fulcio/pkg/certificate"
	siggit "github.com/sigstore/gitsign/pkg/git"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// offline.commit is gitsign v0.17.1's internal/e2e fixture. Its trust
// root comes from sigstore v1.11.0's pkg/tuf/repository/targets.
func TestOfflineReceipt(t *testing.T) {
	raw, err := os.ReadFile("testdata/offline.commit")
	if err != nil {
		t.Fatal(err)
	}
	data, err := siggit.SplitCommit(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	commit := SignedCommit{ID: plumbing.ComputeHash(plumbing.CommitObject, raw).String(), Payload: data.Payload, Signature: data.Gpgsig}
	tr, err := root.NewTrustedRootFromPath("testdata/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := verifierFromRoot(tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("concurrent verifications", func(t *testing.T) {
		for i := range 2 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				got, err := v.IdentifyCommit(t.Context(), commit)
				if err != nil {
					t.Fatal(err)
				}
				if want := (Identity{Issuer: GoogleIssuer, Subject: "billy@chainguard.dev"}); got != want {
					t.Fatalf("IdentifyCommit = %+v, want %+v", got, want)
				}
			})
		}
	})
	// Rekor recorded this signature one second after Fulcio issued the leaf.
	integrated := time.Date(2023, time.June, 5, 16, 11, 20, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		start, end time.Time
		valid      bool
	}{
		{"authority retired after issuance", time.Time{}, integrated.Add(-time.Second), false},
		{"authority not yet trusted", integrated.Add(time.Second), time.Time{}, false},
		{"authority trusted at integration", integrated, integrated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := root.NewTrustedRootFromPath("testdata/trusted_root.json")
			if err != nil {
				t.Fatal(err)
			}
			for _, authority := range tr.FulcioCertificateAuthorities() {
				ca := authority.(*root.FulcioCertificateAuthority)
				if ca.ValidityPeriodEnd.IsZero() {
					ca.ValidityPeriodStart, ca.ValidityPeriodEnd = tc.start, tc.end
				} else {
					// The other CA is trusted at integration but did not issue the leaf.
					ca.ValidityPeriodEnd = time.Time{}
				}
			}
			v, err := verifierFromRoot(tr)
			if err != nil {
				t.Fatal(err)
			}
			got, err := v.IdentifyCommit(t.Context(), commit)
			if tc.valid {
				if want := (Identity{Issuer: GoogleIssuer, Subject: "billy@chainguard.dev"}); err != nil || got != want {
					t.Fatalf("IdentifyCommit = %+v, %v, want %+v", got, err, want)
				}
				return
			}
			if _, unavailable := errors.AsType[*TrustRootError](err); err == nil || unavailable || got != (Identity{}) {
				t.Fatalf("IdentifyCommit = %+v, %v, want no identity and a verification error", got, err)
			}
		})
	}
	clear(tr.RekorLogs())
	_, err = v.IdentifyCommit(t.Context(), commit)
	if _, unavailable := errors.AsType[*TrustRootError](err); unavailable || err == nil {
		t.Fatalf("verification without the receipt's Rekor key = %v, want an invalid signature error", err)
	}
}

func TestCertificateIdentityRequiresIssuerAndSingleSubject(t *testing.T) {
	ext, err := (certificate.Extensions{Issuer: GoogleIssuer}).Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cert x509.Certificate
	}{
		{"missing issuer", x509.Certificate{EmailAddresses: []string{"first@example.org"}}},
		{"missing subject", x509.Certificate{Extensions: ext}},
		{"multiple subjects", x509.Certificate{Extensions: ext, EmailAddresses: []string{"first@example.org", "second@example.org"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := certificateIdentity(&tc.cert); err == nil || got != (Identity{}) {
				t.Fatalf("certificateIdentity = %+v, %v, want an empty identity and an error", got, err)
			}
		})
	}
}
