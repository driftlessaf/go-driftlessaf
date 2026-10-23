/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package testing

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager"
	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/digitorus/timestamp"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	ct "github.com/google/certificate-transparency-go"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	"github.com/google/certificate-transparency-go/x509util"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	prototrustroot "github.com/sigstore/protobuf-specs/gen/pb-go/trustroot/v1"
	rekortilespb "github.com/sigstore/rekor-tiles/v2/pkg/generated/protobuf"
	rekornote "github.com/sigstore/rekor-tiles/v2/pkg/note"
	"github.com/sigstore/rekor-tiles/v2/pkg/types/hashedrekord"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore/pkg/signature"
	f_log "github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/merkle/rfc6962"
	"golang.org/x/mod/sumdb/note"
	"google.golang.org/protobuf/encoding/protojson"
)

// Local serves test-owned Fulcio, Rekor v2 and timestamp endpoints. Its options
// keep the production signer and verifier, including certificate transparency.
// Share one Local among managers that must read one another's attestations.
// It does not test public-service availability or production issuer admission.
type Local struct {
	trusted       root.TrustedMaterial
	signingConfig *root.SigningConfig
	tokenKey      *ecdsa.PrivateKey
	ctKey         *ecdsa.PrivateKey
	leafIssuer    *x509.Certificate
	leafIssuerKey crypto.Signer
	issuer        string
	subject       string
	certificates  atomic.Int64
	entries       atomic.Int64
	timestamps    atomic.Int64
}

// NewLocal starts loopback services that are closed when t finishes.
func NewLocal(t *testing.T) *Local {
	t.Helper()
	now := time.Now()

	rootCert, rootKey, err := ca.GenerateRootCa()
	if err != nil {
		t.Fatalf("GenerateRootCa: %v", err)
	}
	fulcioCert, fulcioKey, err := ca.GenerateFulcioIntermediate(rootCert, rootKey)
	if err != nil {
		t.Fatalf("GenerateFulcioIntermediate: %v", err)
	}
	tsaIntermediate, tsaIntermediateKey, err := ca.GenerateTSAIntermediate(rootCert, rootKey)
	if err != nil {
		t.Fatalf("GenerateTSAIntermediate: %v", err)
	}
	tsaKey := newECDSAKey(t)
	tsaCert, err := ca.GenerateTSALeafCert(now.Add(-5*time.Minute), tsaKey, tsaIntermediate, tsaIntermediateKey)
	if err != nil {
		t.Fatalf("GenerateTSALeafCert: %v", err)
	}
	rekorKey := newECDSAKey(t)

	fs := &Local{tokenKey: newECDSAKey(t), ctKey: newECDSAKey(t), issuer: "https://issuer.test.invalid", subject: fmt.Sprintf("subject-%x", keyID(t, rootKey.Public()))}
	fulcioSrv := serve(t, fulcioHandler(t, fs, fulcioCert, fulcioKey, rootCert))
	tsaSrv := serve(t, tsaHandler(t, fs, tsaCert, tsaKey))
	logID := keyID(t, rekorKey.Public())
	rekorSrv := serve(t, rekorHandler(t, fs, rekorKey, logID))

	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	trusted, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		[]root.CertificateAuthority{&root.FulcioCertificateAuthority{
			Root:                rootCert,
			Intermediates:       []*x509.Certificate{fulcioCert},
			ValidityPeriodStart: start,
			ValidityPeriodEnd:   end,
			URI:                 fulcioSrv.URL,
		}},
		map[string]*root.TransparencyLog{hex.EncodeToString(keyID(t, fs.ctKey.Public())): {ID: keyID(t, fs.ctKey.Public()), PublicKey: fs.ctKey.Public(), HashFunc: crypto.SHA256, SignatureHashFunc: crypto.SHA256, ValidityPeriodStart: start, ValidityPeriodEnd: end}},
		[]root.TimestampingAuthority{&root.SigstoreTimestampingAuthority{
			Root:                rootCert,
			Intermediates:       []*x509.Certificate{tsaIntermediate},
			Leaf:                tsaCert,
			ValidityPeriodStart: start,
			ValidityPeriodEnd:   end,
			URI:                 tsaSrv.URL,
		}},
		map[string]*root.TransparencyLog{hex.EncodeToString(logID): {
			BaseURL:             rekorSrv.URL,
			ID:                  logID,
			ValidityPeriodStart: start,
			ValidityPeriodEnd:   end,
			HashFunc:            crypto.SHA256,
			PublicKey:           rekorKey.Public(),
			SignatureHashFunc:   crypto.SHA256,
		}},
	)
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	anyService := root.ServiceConfiguration{Selector: prototrustroot.ServiceSelector_ANY}
	sc, err := root.NewSigningConfig(root.SigningConfigMediaType02,
		[]root.Service{{URL: fulcioSrv.URL, MajorAPIVersion: 1, ValidityPeriodStart: start}},
		nil,
		[]root.Service{{URL: rekorSrv.URL, MajorAPIVersion: 2, ValidityPeriodStart: start}},
		anyService,
		[]root.Service{{URL: tsaSrv.URL, MajorAPIVersion: 1, ValidityPeriodStart: start}},
		anyService,
	)
	if err != nil {
		t.Fatalf("NewSigningConfig: %v", err)
	}
	fs.leafIssuer, fs.leafIssuerKey = fulcioCert, fulcioKey
	fs.trusted = trusted
	fs.signingConfig = sc
	return fs
}

// Options binds managers to this environment's signing identity and trust roots.
func (fs *Local) Options() []statusmanager.Option {
	return []statusmanager.Option{
		statusmanager.WithOIDCProvider(fs),
		statusmanager.WithExpectedIdentity(cosign.Identity{Subject: fs.issuer + "/" + fs.subject, Issuer: fs.issuer}),
		statusmanager.WithSigningConfig(fs.signingConfig),
		statusmanager.WithTrustedMaterial(fs.trusted),
	}
}

// Enabled reports that the local issuer is available.
func (*Local) Enabled(context.Context) bool { return true }

// Provide issues a short-lived token signed by the local issuer.
func (fs *Local) Provide(_ context.Context, audience string) (string, error) {
	if audience != "sigstore" {
		return "", fmt.Errorf("unexpected audience %q", audience)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: fs.tokenKey}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	now := time.Now()
	return jwt.Signed(signer).Claims(jwt.Claims{Issuer: fs.issuer, Subject: fs.subject, Audience: jwt.Audience{audience}, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), Expiry: jwt.NewNumericDate(now.Add(10 * time.Minute))}).Serialize()
}

func (fs *Local) tokenClaims(token string) (string, string, error) {
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return "", "", err
	}
	var claims jwt.Claims
	if err := parsed.Claims(fs.tokenKey.Public(), &claims); err != nil {
		return "", "", err
	}
	if err := claims.Validate(jwt.Expected{Issuer: fs.issuer, Subject: fs.subject, AnyAudience: jwt.Audience{"sigstore"}, Time: time.Now()}); err != nil {
		return "", "", err
	}
	if claims.Expiry == nil || claims.IssuedAt == nil {
		return "", "", errors.New("token lacks validity bounds")
	}
	return claims.Issuer, claims.Subject, nil
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

// keyID is a transparency log's ID: the SHA-256 of its PKIX public key.
func keyID(t *testing.T, pub crypto.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	sum := sha256.Sum256(der)
	return sum[:]
}

// fulcioHandler serves Fulcio's POST /api/v2/signingCert: it checks the
// proof of possession over the token subject and issues a leaf certificate
// for the requested key.
func fulcioHandler(t *testing.T, fs *Local, issuerCert *x509.Certificate, issuerKey crypto.Signer, rootCert *x509.Certificate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/signingCert" {
			http.NotFound(w, r)
			return
		}
		iss, sub, err := fs.tokenClaims(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var req struct {
			PublicKeyRequest struct {
				PublicKey struct {
					Content string `json:"content"`
				} `json:"publicKey"`
				ProofOfPossession string `json:"proofOfPossession"`
			} `json:"publicKeyRequest"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pub, err := verifyPossession(req.PublicKeyRequest.PublicKey.Content, req.PublicKeyRequest.ProofOfPossession, sub)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		san := iss + "/" + sub
		leaf, err := issueLeaf(pub, san, iss, issuerCert, issuerKey, fs.ctKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fs.certificates.Add(1)

		chain := make([]string, 0, 3)
		for _, c := range []*x509.Certificate{leaf, issuerCert, rootCert} {
			chain = append(chain, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})))
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"signedCertificateEmbeddedSct": map[string]any{"chain": map[string]any{"certificates": chain}},
		})
	})
}

// verifyPossession parses the PEM public key and checks that proof is its
// signature over sub, as Fulcio does.
func verifyPossession(keyPEM, proof, sub string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, errors.New("public key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("unsupported public key type %T", parsed)
	}
	sig, err := base64.StdEncoding.DecodeString(proof)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(sub))
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return nil, errors.New("proof of possession does not verify")
	}
	return pub, nil
}

// Fulcio's OIDC issuer certificate extensions: v1 carries the raw issuer
// string, v2 a DER UTF8String. Public-good Fulcio sets both.
var (
	oidcIssuerV1OID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidcIssuerV2OID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
)

// issueLeaf issues a ten-minute code-signing certificate for pub with a URI
// SAN of san and both OIDC issuer extensions set to iss.
func issueLeaf(pub *ecdsa.PublicKey, san, iss string, issuerCert *x509.Certificate, issuerKey crypto.Signer, ctKey *ecdsa.PrivateKey) (*x509.Certificate, error) {
	sanURI, err := url.Parse(san)
	if err != nil {
		return nil, err
	}
	issuerV2, err := asn1.MarshalWithParams(iss, "utf8")
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		URIs:         []*url.URL{sanURI},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(10 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		ExtraExtensions: []pkix.Extension{
			{Id: oidcIssuerV1OID, Value: []byte(iss)},
			{Id: oidcIssuerV2OID, Value: issuerV2},
		},
	}
	precertDER, err := x509.CreateCertificate(rand.Reader, template, issuerCert, pub, issuerKey)
	if err != nil {
		return nil, err
	}
	precert, err := x509.ParseCertificate(precertDER)
	if err != nil {
		return nil, err
	}
	logKey, err := x509.MarshalPKIXPublicKey(ctKey.Public())
	if err != nil {
		return nil, err
	}
	sct := ct.SignedCertificateTimestamp{SCTVersion: ct.V1, LogID: ct.LogID{KeyID: sha256.Sum256(logKey)}, Timestamp: uint64(now.UnixMilli())}
	entry := ct.LogEntry{Leaf: ct.MerkleTreeLeaf{Version: ct.V1, LeafType: ct.TimestampedEntryLeafType, TimestampedEntry: &ct.TimestampedEntry{Timestamp: sct.Timestamp, EntryType: ct.PrecertLogEntryType, PrecertEntry: &ct.PreCert{IssuerKeyHash: sha256.Sum256(issuerCert.RawSubjectPublicKeyInfo), TBSCertificate: precert.RawTBSCertificate}}}}
	input, err := ct.SerializeSCTSignatureInput(sct, entry)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(input)
	sig, err := ecdsa.SignASN1(rand.Reader, ctKey, digest[:])
	if err != nil {
		return nil, err
	}
	sct.Signature = ct.DigitallySigned{Algorithm: cttls.SignatureAndHashAlgorithm{Hash: cttls.SHA256, Signature: cttls.ECDSA}, Signature: sig}
	list, err := x509util.MarshalSCTsIntoSCTList([]*ct.SignedCertificateTimestamp{&sct})
	if err != nil {
		return nil, err
	}
	wire, err := cttls.Marshal(*list)
	if err != nil {
		return nil, err
	}
	encoded, err := asn1.Marshal(wire)
	if err != nil {
		return nil, err
	}
	template.ExtraExtensions = append(template.ExtraExtensions, pkix.Extension{Id: asn1.ObjectIdentifier(ctx509.OIDExtensionCTSCT), Value: encoded})
	der, err := x509.CreateCertificate(rand.Reader, template, issuerCert, pub, issuerKey)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// rekorHandler serves Rekor v2's POST /api/v2/log/entries: it logs the
// hashedrekord request in a two-leaf tree and returns the
// entry with its inclusion proof and a checkpoint signed by key. The
// checkpoint origin is the host the client reached, which is the hostname
// of the log's BaseURL in the trusted root, as verification requires.
func rekorHandler(t *testing.T, fs *Local, key *ecdsa.PrivateKey, logID []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/log/entries" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req rekortilespb.CreateEntryRequest
		if err := protojson.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hr := req.GetHashedRekordRequestV002()
		if hr == nil {
			http.Error(w, "only hashedrekord v0.0.2 entries are supported", http.StatusBadRequest)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		tle, err := logEntry(hr, key, logID, host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		raw, err := protojson.Marshal(tle)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fs.entries.Add(1)
		writeJSON(t, w, http.StatusCreated, json.RawMessage(raw))
	})
}

// Each request gets an independent two-leaf tree containing the entry twice,
// so inclusion verification exercises a sibling hash. This fixture does not
// model durable append-only storage or consistency between checkpoints.
func logEntry(hr *rekortilespb.HashedRekordRequestV002, key *ecdsa.PrivateKey, logID []byte, origin string) (*protorekor.TransparencyLogEntry, error) {
	algorithms, err := signature.NewAlgorithmRegistryConfig([]protocommon.PublicKeyDetails{protocommon.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256})
	if err != nil {
		return nil, err
	}
	entry, err := hashedrekord.ToLogEntry(hr, algorithms)
	if err != nil {
		return nil, err
	}
	serialized, err := protojson.Marshal(entry)
	if err != nil {
		return nil, err
	}
	canonical, err := jsoncanonicalizer.Transform(serialized)
	if err != nil {
		return nil, err
	}
	leafHash := rfc6962.DefaultHasher.HashLeaf(canonical)
	rootHash := rfc6962.DefaultHasher.HashChildren(leafHash, leafHash)
	sv, err := signature.LoadECDSASignerVerifier(key, crypto.SHA256)
	if err != nil {
		return nil, err
	}
	signer, err := rekornote.NewNoteSigner(context.Background(), origin, sv)
	if err != nil {
		return nil, err
	}
	checkpoint, err := note.Sign(&note.Note{Text: string(f_log.Checkpoint{Origin: origin, Size: 2, Hash: rootHash}.Marshal())}, signer)
	if err != nil {
		return nil, err
	}
	return &protorekor.TransparencyLogEntry{
		LogIndex:    0,
		LogId:       &protocommon.LogId{KeyId: logID},
		KindVersion: &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.2"},
		InclusionProof: &protorekor.InclusionProof{
			LogIndex:   0,
			TreeSize:   2,
			RootHash:   rootHash,
			Hashes:     [][]byte{leafHash},
			Checkpoint: &protorekor.Checkpoint{Envelope: string(checkpoint)},
		},
		CanonicalizedBody: canonical,
	}, nil
}

// tsaHandler serves an RFC3161 timestamp authority.
func tsaHandler(t *testing.T, fs *Local, cert *x509.Certificate, key *ecdsa.PrivateKey) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req, err := timestamp.ParseRequest(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := (&timestamp.Timestamp{
			HashAlgorithm:     req.HashAlgorithm,
			HashedMessage:     req.HashedMessage,
			Time:              time.Now(),
			Nonce:             req.Nonce,
			Policy:            asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 2},
			AddTSACertificate: req.Certificates,
			ExtraExtensions:   req.Extensions,
		}).CreateResponseWithOpts(cert, key, crypto.SHA256)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fs.timestamps.Add(1)
		w.Header().Set("Content-Type", "application/timestamp-reply")
		if _, err := w.Write(resp); err != nil {
			t.Errorf("writing TSA response: %v", err)
		}
	})
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("writing response: %v", err)
	}
}
