/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package payloadcrypt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// aesKeySize is the AES-256 DEK length in bytes.
	aesKeySize = 32
	// gcmIVSize is the AES-GCM nonce length in bytes (96 bits).
	gcmIVSize = 12
	// envelopeVersion marks the wire shape written into sealed fields.
	envelopeVersion = "v1"
)

// dekWrapAAD domain-separates the KMS DEK wrap so a wrapped DEK minted for a
// different subsystem under the same KEK cannot be substituted. Readers must
// pass the same AAD to KMS Decrypt (see DEKWrapAAD).
var dekWrapAAD = []byte("driftlessaf/agent-trace/payload-dek")

// Sealed is the JSON wire shape written in place of a sensitive field. All
// binary fields are base64. The recorder loads it as native JSON (JSON columns)
// or as a JSON string (STRING columns); either way it is opaque to BigQuery.
type Sealed struct {
	// Enc is the envelope-version marker; also lets consumers detect ciphertext.
	Enc string `json:"driftlessaf_enc"`
	// KEK is the KMS crypto-key resource name whose Decrypt recovers the DEK.
	KEK string `json:"kek"`
	// WrappedDEK is the base64 KMS-wrapped per-event AES-256 DEK.
	WrappedDEK string `json:"wdek"`
	// IV is the base64 AES-GCM nonce for this field.
	IV string `json:"iv"`
	// Ciphertext is the base64 AES-GCM ciphertext (including the auth tag).
	Ciphertext string `json:"ct"`
}

// WrapFunc wraps a freshly-minted DEK and returns the wrapped bytes. The KMS
// implementation is a symmetric Encrypt under the KEK; tests inject a fake so
// the package's crypto is exercisable without GCP.
type WrapFunc func(ctx context.Context, dek []byte) (wrapped []byte, err error)

// Encryptor mints per-event sealing sessions. Safe for concurrent use.
type Encryptor struct {
	keyName string
	wrap    WrapFunc
}

// New returns an Encryptor backed by the given DEK-wrap callback. keyName is the
// KMS crypto-key resource name recorded in each envelope so readers know which
// key to Decrypt against.
func New(keyName string, wrap WrapFunc) (*Encryptor, error) {
	if keyName == "" {
		return nil, errors.New("payloadcrypt: keyName is required")
	}
	if wrap == nil {
		return nil, errors.New("payloadcrypt: wrap func is required")
	}
	return &Encryptor{keyName: keyName, wrap: wrap}, nil
}

// The Cloud KMS-backed WrapFunc (the production wrap) lives in the sibling
// kmsseal package, so this package stays free of cloud.google.com/go/kms and can
// be imported transitively (via agenttrace) without pulling the KMS SDK into
// every agent-trace consumer's dependency graph.

// Session seals many fields under a single KMS-wrapped DEK — one KMS Encrypt per
// session. Create one per CloudEvent (via NewSession) so a trace's or span's
// fields share a DEK and per-event KMS calls stay O(1) regardless of how many
// tool calls or reasoning blocks a trace contains.
type Session struct {
	keyName    string
	wrappedDEK string
	gcm        cipher.AEAD
}

// NewSession mints a fresh DEK, wraps it once via the configured WrapFunc, and
// returns a Session that seals fields locally under that DEK.
func (e *Encryptor) NewSession(ctx context.Context) (*Session, error) {
	dek := make([]byte, aesKeySize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("payloadcrypt: generate DEK: %w", err)
	}
	wrapped, err := e.wrap(ctx, dek)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: aes-gcm: %w", err)
	}
	return &Session{
		keyName:    e.keyName,
		wrappedDEK: base64.StdEncoding.EncodeToString(wrapped),
		gcm:        gcm,
	}, nil
}

// envelopeAAD binds the GCM tag to the envelope's version and KEK, so a valid
// {iv, ct} blob cannot be relocated into an envelope declaring a different
// version or key without failing authentication. Derived solely from the
// envelope's own self-describing fields, so Open reconstructs it without extra
// inputs (keeping break-glass decryption self-contained). It does NOT bind field
// identity: because all fields in one event share a DEK, swapping two fields'
// blobs within the same envelope still authenticates — that requires write
// access to the row, which is outside the read-only-dataViewer threat model.
func envelopeAAD(version, kek string) []byte {
	return []byte(version + "\x00" + kek)
}

// Seal AES-256-GCM-encrypts plaintext under the session DEK with a fresh nonce
// and returns the JSON envelope. The envelope version and KEK are bound as GCM
// additional authenticated data (see envelopeAAD).
func (s *Session) Seal(plaintext []byte) (json.RawMessage, error) {
	iv := make([]byte, gcmIVSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, fmt.Errorf("payloadcrypt: generate iv: %w", err)
	}
	ct := s.gcm.Seal(nil, iv, plaintext, envelopeAAD(envelopeVersion, s.keyName))
	out, err := json.Marshal(Sealed{
		Enc:        envelopeVersion,
		KEK:        s.keyName,
		WrappedDEK: s.wrappedDEK,
		IV:         base64.StdEncoding.EncodeToString(iv),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	})
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: marshal envelope: %w", err)
	}
	return out, nil
}

// DEKWrapAAD returns a copy of the additional-authenticated-data bound to the
// KMS DEK wrap, so reader/break-glass code passes the same AAD to KMS Decrypt.
func DEKWrapAAD() []byte { return append([]byte(nil), dekWrapAAD...) }

// Open decrypts a sealed envelope. unwrapDEK recovers the AES DEK from the
// KMS-wrapped DEK — typically a KMS Decrypt against kek (the envelope's declared
// Sealed.KEK) with DEKWrapAAD. kek is passed to the callback so a reader spanning
// envelopes minted under different keys (e.g. across KEK rotation, all living in
// the same dataset) can Decrypt against the right key without re-parsing the
// envelope. Open stays GCP-free so tests and readers can reuse the AES-GCM/base64
// logic without pulling in cloud.google.com/go/kms.
func Open(envelope []byte, unwrapDEK func(kek string, wrapped []byte) (dek []byte, err error)) ([]byte, error) {
	var s Sealed
	if err := json.Unmarshal(envelope, &s); err != nil {
		return nil, fmt.Errorf("payloadcrypt: parse envelope: %w", err)
	}
	if s.Enc != envelopeVersion {
		return nil, fmt.Errorf("payloadcrypt: unknown envelope version %q", s.Enc)
	}
	wrapped, err := base64.StdEncoding.DecodeString(s.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: decode wrapped dek: %w", err)
	}
	iv, err := base64.StdEncoding.DecodeString(s.IV)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: decode iv: %w", err)
	}
	if len(iv) != gcmIVSize {
		return nil, fmt.Errorf("payloadcrypt: iv has wrong length: got %d, want %d", len(iv), gcmIVSize)
	}
	ct, err := base64.StdEncoding.DecodeString(s.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: decode ciphertext: %w", err)
	}
	dek, err := unwrapDEK(s.KEK, wrapped)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: unwrap dek: %w", err)
	}
	if l := len(dek); l != aesKeySize {
		return nil, fmt.Errorf("payloadcrypt: unwrapped dek has wrong length: got %d, want %d", l, aesKeySize)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: aes-gcm: %w", err)
	}
	plaintext, err := gcm.Open(nil, iv, ct, envelopeAAD(s.Enc, s.KEK))
	if err != nil {
		return nil, fmt.Errorf("payloadcrypt: aes-gcm open: %w", err)
	}
	return plaintext, nil
}
