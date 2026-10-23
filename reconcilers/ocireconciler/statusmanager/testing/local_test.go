/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package testing

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type loopbackTransport struct{ inner http.RoundTripper }

func (tr loopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	addr, err := netip.ParseAddr(req.URL.Hostname())
	if err != nil || !addr.IsLoopback() {
		return nil, fmt.Errorf("test forbids non-loopback HTTP host %q", req.URL.Hostname())
	}
	return tr.inner.RoundTrip(req)
}

func TestMain(m *testing.M) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	http.DefaultTransport = loopbackTransport{inner: transport}
	os.Exit(m.Run())
}

type localStatus struct {
	Message string `json:"message"`
}

func (localStatus) PredicateType() string { return "https://status.test/local" }

func TestLocalRoundTrip(t *testing.T) {
	fs := NewLocal(t)
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	opts := append(fs.Options(), statusmanager.WithRepositoryOverride(strings.TrimPrefix(srv.URL, "http://")+"/status"))
	mgr, err := statusmanager.New[localStatus](t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := name.NewDigest("example.invalid/artifact@sha256:" + strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	session := mgr.NewSession(digest)
	want := &statusmanager.Status[localStatus]{Details: localStatus{Message: t.Name()}}
	if err := session.SetActualState(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	got, err := session.ObservedState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Details != want.Details {
		t.Fatalf("observed = %#v, want %#v", got, want)
	}
	first := [3]int64{fs.certificates.Load(), fs.entries.Load(), fs.timestamps.Load()}
	if first != [3]int64{1, 1, 1} {
		t.Fatalf("signing requests = %v, want [1 1 1]", first)
	}
	if err := session.SetActualState(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if got := [3]int64{fs.certificates.Load(), fs.entries.Load(), fs.timestamps.Load()}; got != first {
		t.Fatalf("identical write signed again: got %v, want %v", got, first)
	}
	want.Details.Message += " updated"
	if err := session.SetActualState(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if got := [3]int64{fs.certificates.Load(), fs.entries.Load(), fs.timestamps.Load()}; got != [3]int64{1, 2, 2} {
		t.Fatalf("cached-cert signing requests = %v, want [1 2 2]", got)
	}
}

func TestLocalRejectsInvalidEvidence(t *testing.T) {
	fs := NewLocal(t)
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	repo := strings.TrimPrefix(srv.URL, "http://") + "/valid"
	opts := append(fs.Options(), statusmanager.WithRepositoryOverride(repo))
	writer, err := statusmanager.New[localStatus](t.Context(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := empty.Image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewDigest(repo + "@" + subject.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, empty.Image); err != nil {
		t.Fatal(err)
	}
	want := localStatus{Message: t.Name()}
	if err := writer.NewSession(ref).SetActualState(t.Context(), &statusmanager.Status[localStatus]{Details: want}); err != nil {
		t.Fatal(err)
	}
	bundles, _, err := cosign.GetBundles(t.Context(), ref, nil)
	if err != nil || len(bundles) != 1 {
		t.Fatalf("bundles = %d, %v; want one", len(bundles), err)
	}
	original := bundles[0].Bundle
	originalCert, err := x509.ParseCertificate(original.GetVerificationMaterial().GetCertificate().GetRawBytes())
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		change  func(*testing.T, *protobundle.Bundle)
		options func(*testing.T) []statusmanager.Option
		valid   bool
	}{
		{name: "valid evidence", valid: true},
		{name: "wrong subject", options: func(t *testing.T) []statusmanager.Option {
			return []statusmanager.Option{statusmanager.WithExpectedIdentity(cosign.Identity{Issuer: fs.issuer, Subject: fs.issuer + "/other"})}
		}},
		{name: "wrong issuer", options: func(t *testing.T) []statusmanager.Option {
			return []statusmanager.Option{statusmanager.WithExpectedIdentity(cosign.Identity{Issuer: "https://other.invalid", Subject: fs.issuer + "/" + fs.subject})}
		}},
		{name: "untrusted authority", options: func(t *testing.T) []statusmanager.Option {
			return []statusmanager.Option{statusmanager.WithTrustedMaterial(NewLocal(t).trusted)}
		}},
		{name: "changed payload", change: func(t *testing.T, b *protobundle.Bundle) {
			b.GetDsseEnvelope().Payload = append(b.GetDsseEnvelope().Payload, ' ')
		}},
		{name: "changed signature", change: func(t *testing.T, b *protobundle.Bundle) { b.GetDsseEnvelope().Signatures[0].Sig[0] ^= 1 }},
		{name: "missing log", change: func(t *testing.T, b *protobundle.Bundle) { b.VerificationMaterial.TlogEntries = nil }},
		{name: "changed log proof", change: func(t *testing.T, b *protobundle.Bundle) {
			// Rekor v2 verifies siblings against the signed checkpoint's root.
			b.VerificationMaterial.TlogEntries[0].InclusionProof.Hashes[0][0] ^= 1
		}},
		{name: "changed checkpoint", change: func(t *testing.T, b *protobundle.Bundle) {
			b.VerificationMaterial.TlogEntries[0].InclusionProof.Checkpoint.Envelope = "invalid checkpoint"
		}},
		{name: "missing timestamp", change: func(t *testing.T, b *protobundle.Bundle) { b.VerificationMaterial.TimestampVerificationData = nil }},
		{name: "changed timestamp", change: func(t *testing.T, b *protobundle.Bundle) {
			ts := b.VerificationMaterial.TimestampVerificationData.Rfc3161Timestamps[0]
			ts.SignedTimestamp[len(ts.SignedTimestamp)-1] ^= 1
		}},
		{name: "missing SCT", change: func(t *testing.T, b *protobundle.Bundle) {
			b.VerificationMaterial.GetCertificate().RawBytes = reissueCertificate(t, fs, originalCert, false)
		}},
		{name: "changed SCT", change: func(t *testing.T, b *protobundle.Bundle) {
			b.VerificationMaterial.GetCertificate().RawBytes = reissueCertificate(t, fs, originalCert, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := proto.Clone(original).(*protobundle.Bundle)
			if tc.change != nil {
				tc.change(t, b)
			}
			raw, err := protojson.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			target := ref.Context().RegistryStr() + "/" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
			dest, err := name.NewDigest(target + "@" + ref.DigestStr())
			if err != nil {
				t.Fatal(err)
			}
			if err := remote.Write(dest, empty.Image); err != nil {
				t.Fatal(err)
			}
			if err := ociremote.WriteAttestationNewBundleFormat(dest, raw, (localStatus{}).PredicateType()); err != nil {
				t.Fatal(err)
			}
			readOpts := append(fs.Options(), statusmanager.WithRepositoryOverride(target))
			if tc.options != nil {
				readOpts = append(readOpts, tc.options(t)...)
			}
			reader, err := statusmanager.NewReadOnly[localStatus](t.Context(), readOpts...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.NewSession(dest).ObservedState(t.Context())
			if tc.valid {
				if err != nil || got == nil || got.Details != want {
					t.Fatalf("valid evidence produced state %#v (error %v)", got, err)
				}
				return
			}
			if got != nil {
				t.Fatalf("invalid evidence produced state %#v (error %v)", got, err)
			}
		})
	}
	t.Run("wrong artifact digest", func(t *testing.T) {
		subject, err := empty.Index.Digest()
		if err != nil {
			t.Fatal(err)
		}
		dest, err := name.NewDigest(repo + "@" + subject.String())
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.WriteIndex(dest, empty.Index); err != nil {
			t.Fatal(err)
		}
		raw, err := protojson.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		if err := ociremote.WriteAttestationNewBundleFormat(dest, raw, (localStatus{}).PredicateType()); err != nil {
			t.Fatal(err)
		}
		got, err := writer.NewSession(dest).ObservedState(t.Context())
		if got != nil {
			t.Fatalf("wrong artifact digest produced state %#v (error %v)", got, err)
		}
	})
}

func reissueCertificate(t *testing.T, fs *Local, original *x509.Certificate, corrupt bool) []byte {
	t.Helper()
	template := *original
	template.ExtraExtensions = nil
	for _, ext := range original.Extensions {
		if ext.Id.Equal(asn1.ObjectIdentifier(ctx509.OIDExtensionCTSCT)) {
			if !corrupt {
				continue
			}
			ext.Value = append([]byte(nil), ext.Value...)
			ext.Value[len(ext.Value)-1] ^= 1
		}
		template.ExtraExtensions = append(template.ExtraExtensions, ext)
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, fs.leafIssuer, original.PublicKey, fs.leafIssuerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	// The CA signature remains valid: refusal must include the absent/bad SCT.
	if err := cert.CheckSignatureFrom(fs.leafIssuer); err != nil {
		t.Fatal(err)
	}
	if err := verify.VerifySignedCertificateTimestamp([][]*x509.Certificate{{cert, fs.leafIssuer}}, 1, fs.trusted); err == nil {
		t.Fatal("invalid SCT verified")
	}
	return der
}

func TestLocalTokenAndPossessionRefusals(t *testing.T) {
	fs := NewLocal(t)
	otherKey := newECDSAKey(t)
	now := time.Now()
	for _, tc := range []struct {
		name                      string
		key                       *ecdsa.PrivateKey
		issuer, subject, audience string
		expires                   time.Time
		valid                     bool
	}{
		{"valid", fs.tokenKey, fs.issuer, fs.subject, "sigstore", now.Add(time.Minute), true},
		{"wrong key", otherKey, fs.issuer, fs.subject, "sigstore", now.Add(time.Minute), false},
		{"wrong issuer", fs.tokenKey, "https://other.invalid", fs.subject, "sigstore", now.Add(time.Minute), false},
		{"wrong subject", fs.tokenKey, fs.issuer, "other", "sigstore", now.Add(time.Minute), false},
		{"wrong audience", fs.tokenKey, fs.issuer, fs.subject, "other", now.Add(time.Minute), false},
		{"expired", fs.tokenKey, fs.issuer, fs.subject, "sigstore", now.Add(-time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: tc.key}, nil)
			if err != nil {
				t.Fatal(err)
			}
			token, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: tc.issuer, Subject: tc.subject, Audience: jwt.Audience{tc.audience}, IssuedAt: jwt.NewNumericDate(now.Add(-2 * time.Hour)), Expiry: jwt.NewNumericDate(tc.expires)}).Serialize()
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = fs.tokenClaims(token)
			if (err == nil) != tc.valid {
				t.Fatalf("token accepted=%t, want %t", err == nil, tc.valid)
			}
		})
	}
	publicDER, err := x509.MarshalPKIXPublicKey(otherKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	h := sha256.Sum256([]byte(fs.subject))
	proof, err := ecdsa.SignASN1(rand.Reader, otherKey, h[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyPossession(publicPEM, base64.StdEncoding.EncodeToString(proof), fs.subject); err != nil {
		t.Fatal(err)
	}
	proof[0] ^= 1
	if _, err := verifyPossession(publicPEM, base64.StdEncoding.EncodeToString(proof), fs.subject); err == nil {
		t.Fatal("corrupt possession proof accepted")
	}
}
