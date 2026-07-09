/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package payloadcrypt

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// xorWrap is a stand-in for a KMS symmetric wrap: reversible and deterministic
// enough to prove the envelope round-trips without reaching a real KMS. It is
// NOT cryptographically meaningful — production uses kmsseal.New.
func xorWrap(_ context.Context, dek []byte) ([]byte, error) {
	out := make([]byte, len(dek))
	for i, b := range dek {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

func xorUnwrap(_ string, wrapped []byte) ([]byte, error) {
	out := make([]byte, len(wrapped))
	for i, b := range wrapped {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

func TestSealOpenRoundTrip(t *testing.T) {
	const keyName = "projects/p/locations/us-central1/keyRings/argos/cryptoKeys/agent-trace-payload-key"
	enc, err := New(keyName, xorWrap)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess, err := enc.NewSession(t.Context())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	cases := map[string][]byte{
		"prompt":    []byte(`"analyze CVE-2025-1234 in requests 2.28.0"`),
		"result":    []byte(`{"verdict":"patched","cwe":"CWE-79"}`),
		"empty":     []byte(``),
		"unicode":   []byte(`"día — 例 — 🔐"`),
		"largeblob": bytes.Repeat([]byte("A"), 200*1024), // large plaintext round-trips (AES-GCM handles arbitrary size; only the 32-byte DEK is wrapped)
	}

	for name, plaintext := range cases {
		t.Run(name, func(t *testing.T) {
			envelope, err := sess.Seal(plaintext)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}

			// The envelope must be valid JSON carrying the marker + KEK, and must
			// not contain the plaintext.
			var s Sealed
			if err := json.Unmarshal(envelope, &s); err != nil {
				t.Fatalf("envelope is not valid JSON: %v", err)
			}
			if s.Enc != envelopeVersion {
				t.Errorf("marker = %q, want %q", s.Enc, envelopeVersion)
			}
			if s.KEK != keyName {
				t.Errorf("KEK = %q, want %q", s.KEK, keyName)
			}
			if len(plaintext) > 0 && bytes.Contains(envelope, plaintext) {
				t.Error("envelope leaks plaintext")
			}

			got, err := Open(envelope, xorUnwrap)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Errorf("round-trip mismatch:\n got %q\nwant %q", got, plaintext)
			}
		})
	}
}

func TestSealFreshNoncePerCall(t *testing.T) {
	enc, err := New("k", xorWrap)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess, err := enc.NewSession(t.Context())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	pt := []byte("same plaintext")
	a, _ := sess.Seal(pt)
	b, _ := sess.Seal(pt)
	if bytes.Equal(a, b) {
		t.Error("two seals of identical plaintext produced identical envelopes (nonce reuse)")
	}
}

func TestOpenRejectsTampered(t *testing.T) {
	enc, _ := New("k", xorWrap)
	sess, _ := enc.NewSession(t.Context())
	envelope, _ := sess.Seal([]byte("secret"))

	var s Sealed
	if err := json.Unmarshal(envelope, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Flip the ciphertext: GCM auth must fail on Open.
	s.Ciphertext = "AAAA" + s.Ciphertext[4:]
	tampered, _ := json.Marshal(s)
	if _, err := Open(tampered, xorUnwrap); err == nil {
		t.Error("Open accepted tampered ciphertext")
	}
}

func TestOpenRejectsRelocatedEnvelopeMetadata(t *testing.T) {
	enc, _ := New("projects/p/locations/l/keyRings/r/cryptoKeys/k1", xorWrap)
	sess, _ := enc.NewSession(t.Context())
	envelope, _ := sess.Seal([]byte("secret"))

	var s Sealed
	if err := json.Unmarshal(envelope, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Rewrite the KEK field: the envelope AAD binds version+KEK, so Open must
	// fail rather than decrypt a blob relocated under a different declared key.
	s.KEK = "projects/p/locations/l/keyRings/r/cryptoKeys/k2"
	relocated, _ := json.Marshal(s)
	if _, err := Open(relocated, xorUnwrap); err == nil {
		t.Error("Open accepted a blob whose declared KEK was changed (AAD not bound)")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("", xorWrap); err == nil {
		t.Error("New accepted empty keyName")
	}
	if _, err := New("k", nil); err == nil {
		t.Error("New accepted nil wrap")
	}
}
