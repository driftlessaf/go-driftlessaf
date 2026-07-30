/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package kmsseal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	gax "github.com/googleapis/gax-go/v2"
)

const testKEK = "projects/p/locations/l/keyRings/r/cryptoKeys/k"

// fakeKMS is a reversible kmsClient stand-in: Encrypt XORs the plaintext with
// a fixed pad (so Decrypt is its own inverse) and both directions record the
// full request, so tests can assert what actually goes over the wire.
type fakeKMS struct {
	pad         byte
	failEncrypt error
	failDecrypt error

	encrypts    []*kmspb.EncryptRequest
	decrypts    []*kmspb.DecryptRequest
	lastWrapped []byte
}

func (f *fakeKMS) Encrypt(_ context.Context, req *kmspb.EncryptRequest, _ ...gax.CallOption) (*kmspb.EncryptResponse, error) {
	if f.failEncrypt != nil {
		return nil, f.failEncrypt
	}
	f.encrypts = append(f.encrypts, req)
	out := make([]byte, len(req.GetPlaintext()))
	for i, b := range req.GetPlaintext() {
		out[i] = b ^ f.pad
	}
	f.lastWrapped = out
	return &kmspb.EncryptResponse{Ciphertext: out}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, req *kmspb.DecryptRequest, _ ...gax.CallOption) (*kmspb.DecryptResponse, error) {
	if f.failDecrypt != nil {
		return nil, f.failDecrypt
	}
	f.decrypts = append(f.decrypts, req)
	out := make([]byte, len(req.GetCiphertext()))
	for i, b := range req.GetCiphertext() {
		out[i] = b ^ f.pad
	}
	return &kmspb.DecryptResponse{Plaintext: out}, nil
}

func testSealer(f *fakeKMS) *Sealer { return newWithClient(testKEK, f) }

func TestSealOpenRoundTrip(t *testing.T) {
	f := &fakeKMS{pad: 0x5a}
	s := testSealer(f)

	plaintext := make([]byte, 4096)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealed, err := s.Seal(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext[:64]) {
		t.Fatal("sealed blob contains plaintext bytes")
	}
	got, err := s.Open(t.Context(), sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip: got %d bytes differing from the sealed plaintext", len(got))
	}
	if len(f.encrypts) != 1 || len(f.decrypts) != 1 {
		t.Errorf("KMS calls (wrap/unwrap): got = %d/%d, want 1/1 (envelope keeps KMS per-wrap, not per-byte)",
			len(f.encrypts), len(f.decrypts))
	}
}

// The AAD is the property doing the cross-caller binding work — pin that both
// wire directions actually carry it (and the key name), so dropping it can
// never pass the suite while failing only against live KMS.
func TestKMSWrapCarriesTheBindingAAD(t *testing.T) {
	f := &fakeKMS{pad: 0x2c}
	s := testSealer(f)
	sealed, err := s.Seal(t.Context(), []byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s.Open(t.Context(), sealed); err != nil {
		t.Fatalf("Open: %v", err)
	}

	enc, dec := f.encrypts[0], f.decrypts[0]
	if string(enc.GetAdditionalAuthenticatedData()) != dekWrapAAD {
		t.Errorf("Encrypt AAD: got = %q, want = %q", enc.GetAdditionalAuthenticatedData(), dekWrapAAD)
	}
	if string(dec.GetAdditionalAuthenticatedData()) != dekWrapAAD {
		t.Errorf("Decrypt AAD: got = %q, want = %q", dec.GetAdditionalAuthenticatedData(), dekWrapAAD)
	}
	if enc.GetName() != testKEK || dec.GetName() != testKEK {
		t.Errorf("KMS key name (wrap/unwrap): got = %q/%q, want both %q", enc.GetName(), dec.GetName(), testKEK)
	}
}

func TestSealUsesAFreshDEKPerCall(t *testing.T) {
	f := &fakeKMS{pad: 0x11}
	s := testSealer(f)
	if _, err := s.Seal(t.Context(), []byte("one")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	first := bytes.Clone(f.lastWrapped)
	if _, err := s.Seal(t.Context(), []byte("two")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(first, f.lastWrapped) {
		t.Error("two Seals wrapped the same DEK; want a fresh random DEK per call")
	}
}

func TestOpenRejectsTamperedBlob(t *testing.T) {
	s := testSealer(&fakeKMS{pad: 0x33})
	sealed, err := s.Seal(t.Context(), []byte("the reference bundle grants root in the guest"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	var b blob
	if err := json.Unmarshal(sealed, &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b.Ciphertext[len(b.Ciphertext)/2] ^= 0x01
	tampered, err := json.Marshal(&b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := s.Open(t.Context(), tampered); err == nil {
		t.Error("Open(tampered): got nil error, want a hard authentication failure")
	}
}

func TestOpenRejectsForeignKEK(t *testing.T) {
	f := &fakeKMS{pad: 0x44}
	sealed, err := newWithClient("projects/p/locations/l/keyRings/r/cryptoKeys/other", f).Seal(t.Context(), []byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := testSealer(f).Open(t.Context(), sealed); err == nil {
		t.Error("Open(foreign KEK): got nil error, want rejection before any unwrap")
	}
	if len(f.decrypts) != 0 {
		t.Errorf("unwraps: got = %d, want 0 (fail closed before touching KMS)", len(f.decrypts))
	}
}

func TestOpenRejectsUnknownVersion(t *testing.T) {
	f := &fakeKMS{pad: 0x55}
	s := testSealer(f)
	sealed, err := s.Seal(t.Context(), []byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	var b blob
	if err := json.Unmarshal(sealed, &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b.V = blobVersion + 1
	future, err := json.Marshal(&b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := s.Open(t.Context(), future); err == nil {
		t.Error("Open(future version): got nil error, want a clean unsupported-version rejection")
	}
	if len(f.decrypts) != 0 {
		t.Errorf("unwraps: got = %d, want 0 (version check precedes KMS)", len(f.decrypts))
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	s := testSealer(&fakeKMS{})
	if _, err := s.Open(t.Context(), []byte("not a sealed blob")); err == nil {
		t.Error("Open(garbage): got nil error, want a parse failure")
	}
}

func TestSealSurfacesWrapFailure(t *testing.T) {
	s := testSealer(&fakeKMS{failEncrypt: errors.New("kms unavailable")})
	if _, err := s.Seal(t.Context(), []byte("x")); err == nil {
		t.Error("Seal with failing wrap: got nil error, want the KMS failure surfaced")
	}
}

func TestOpenSurfacesUnwrapFailure(t *testing.T) {
	f := &fakeKMS{pad: 0x22}
	s := testSealer(f)
	sealed, err := s.Seal(t.Context(), []byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	f.failDecrypt = errors.New("permission denied")
	if _, err := s.Open(t.Context(), sealed); err == nil {
		t.Error("Open with failing unwrap: got nil error, want the KMS failure surfaced")
	}
}

func TestSealOpenEmptyPlaintext(t *testing.T) {
	s := testSealer(&fakeKMS{pad: 0x66})
	sealed, err := s.Seal(t.Context(), nil)
	if err != nil {
		t.Fatalf("Seal(nil): %v", err)
	}
	got, err := s.Open(t.Context(), sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("round trip of empty plaintext: got %d bytes, want 0", len(got))
	}
}

func TestOpenRejectsShortDEK(t *testing.T) {
	f := &fakeKMS{pad: 0x77}
	s := testSealer(f)
	sealed, err := s.Seal(t.Context(), []byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// An unwrap that returns a 16-byte key must fail the AES-256 contract, not
	// silently select AES-128.
	s2 := newWithClient(testKEK, &shortDEKClient{fakeKMS: f})
	if _, err := s2.Open(t.Context(), sealed); err == nil {
		t.Error("Open with a 16-byte unwrapped DEK: got nil error, want the length rejection")
	}
}

// shortDEKClient decrypts to a 16-byte key regardless of input.
type shortDEKClient struct{ *fakeKMS }

func (s *shortDEKClient) Decrypt(context.Context, *kmspb.DecryptRequest, ...gax.CallOption) (*kmspb.DecryptResponse, error) {
	return &kmspb.DecryptResponse{Plaintext: make([]byte, 16)}, nil
}

func TestNewRequiresKeyName(t *testing.T) {
	if _, _, err := New(t.Context(), ""); err == nil {
		t.Error("New(empty key): got nil error, want rejection")
	}
}
