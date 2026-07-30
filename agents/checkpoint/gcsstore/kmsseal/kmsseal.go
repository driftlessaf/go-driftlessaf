/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package kmsseal

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	gax "github.com/googleapis/gax-go/v2"

	"chainguard.dev/driftlessaf/agents/checkpoint/gcsstore"
)

// dekWrapAAD binds every KMS DEK wrap to this package's purpose, so a
// ciphertext wrapped here cannot be unwrapped through another caller of the
// same KEK (and vice versa). Constant by design: the blob is self-contained,
// so the AAD cannot carry per-object context without breaking Open.
const dekWrapAAD = "driftlessaf/checkpoint/gcsstore/kmsseal:dek:v1"

// blobVersion is the current on-disk blob format. Open rejects versions it
// does not know with a clean error rather than a raw KMS/GCM failure, so the
// format can evolve without stranding old readers on opaque errors.
const blobVersion = 1

// blob is the single stored object: the wrapped DEK plus the locally
// AEAD-encrypted payload (see the gcsstore package doc's SEAL/OPEN diagram).
type blob struct {
	// V is the blob format version; Open rejects versions it doesn't know.
	V int `json:"v"`
	// KEK names the KMS crypto key the DEK is wrapped under, recorded so Open
	// can fail closed on a blob sealed under a different key.
	KEK string `json:"kek"`
	// WrappedDEK is the KMS-encrypted data-encryption key.
	WrappedDEK []byte `json:"wrapped_dek"`
	// Nonce is the AES-GCM nonce for Ciphertext.
	Nonce []byte `json:"nonce"`
	// Ciphertext is the AES-256-GCM sealed payload (auth tag included).
	Ciphertext []byte `json:"ciphertext"`
}

// kmsClient is the subset of *kms.KeyManagementClient the sealer drives — the
// seam tests replace so they can assert what actually goes over the wire
// (the AAD in particular; it is the cross-caller binding property).
type kmsClient interface {
	Encrypt(ctx context.Context, req *kmspb.EncryptRequest, opts ...gax.CallOption) (*kmspb.EncryptResponse, error)
	Decrypt(ctx context.Context, req *kmspb.DecryptRequest, opts ...gax.CallOption) (*kmspb.DecryptResponse, error)
}

var _ kmsClient = (*kms.KeyManagementClient)(nil)

// Sealer is a [gcsstore.Sealer] that envelope-encrypts each payload: a fresh
// random AES-256-GCM DEK per Seal, wrapped by a Cloud KMS symmetric KEK that
// never leaves KMS. KMS traffic stays per-key-wrap rather than per-megabyte,
// and the GCM tag makes stored bytes tamper-evident: Open of a modified blob
// is a hard failure. Substitution of one WHOLE valid blob for another is not
// detectable at this layer — see the package doc for the caller-side binding
// that closes it. Construct with [New]; the zero value is unusable.
type Sealer struct {
	keyName string
	client  kmsClient
}

var _ gcsstore.Sealer = (*Sealer)(nil)

// New returns a Sealer whose DEKs are wrapped by the Cloud KMS symmetric key
// keyName (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>). Sealing
// requires Encrypt on the key, opening requires Decrypt — grant the caller
// only the direction(s) it needs. The returned close func releases the KMS
// client; call it at process shutdown.
func New(ctx context.Context, keyName string) (*Sealer, func() error, error) {
	if keyName == "" {
		return nil, nil, errors.New("kmsseal: keyName is required")
	}
	client, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("kmsseal: building KMS client: %w", err)
	}
	return newWithClient(keyName, client), client.Close, nil
}

// newWithClient wires a Sealer over an explicit KMS client — the seam tests
// use with a fake, keeping the AAD and request shape under test.
func newWithClient(keyName string, client kmsClient) *Sealer {
	return &Sealer{keyName: keyName, client: client}
}

// Seal envelope-encrypts plaintext: fresh 256-bit DEK, AES-GCM with a random
// nonce, DEK wrapped via KMS (with the package's purpose-binding AAD), all
// stored as one self-contained JSON blob.
func (s *Sealer) Seal(ctx context.Context, plaintext []byte) ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("kmsseal: generating DEK: %w", err)
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("kmsseal: generating nonce: %w", err)
	}
	resp, err := s.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:                        s.keyName,
		Plaintext:                   dek,
		AdditionalAuthenticatedData: []byte(dekWrapAAD),
	})
	if err != nil {
		return nil, fmt.Errorf("kmsseal: wrap DEK: %w", err)
	}
	return json.Marshal(&blob{
		V:          blobVersion,
		KEK:        s.keyName,
		WrappedDEK: resp.GetCiphertext(),
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, plaintext, nil),
	})
}

// Open reverses Seal: unwrap the DEK via KMS, then decrypt AND authenticate
// the payload. A blob of an unknown format version, sealed under a different
// KEK, or whose stored bytes were modified is a hard error — never silently
// returned data.
func (s *Sealer) Open(ctx context.Context, ciphertext []byte) ([]byte, error) {
	var b blob
	if err := json.Unmarshal(ciphertext, &b); err != nil {
		return nil, fmt.Errorf("kmsseal: parsing sealed blob: %w", err)
	}
	if b.V != blobVersion {
		return nil, fmt.Errorf("kmsseal: unsupported blob version %d (this reader knows %d)", b.V, blobVersion)
	}
	if b.KEK != s.keyName {
		return nil, fmt.Errorf("kmsseal: blob sealed under KEK %q, not this sealer's %q", b.KEK, s.keyName)
	}
	resp, err := s.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:                        s.keyName,
		Ciphertext:                  b.WrappedDEK,
		AdditionalAuthenticatedData: []byte(dekWrapAAD),
	})
	if err != nil {
		return nil, fmt.Errorf("kmsseal: unwrap DEK: %w", err)
	}
	dek := resp.GetPlaintext()
	// The unwrap is trusted KMS output, but hold the AES-256 contract anyway:
	// a shorter key would silently select AES-128/192 rather than fail.
	if len(dek) != 32 {
		return nil, fmt.Errorf("kmsseal: unwrapped DEK is %d bytes, want 32", len(dek))
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, b.Nonce, b.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("kmsseal: decrypting payload (tampered or corrupt blob): %w", err)
	}
	return plaintext, nil
}

// newAEAD builds the AES-256-GCM AEAD for a 32-byte DEK.
func newAEAD(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("kmsseal: building cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("kmsseal: building AEAD: %w", err)
	}
	return aead, nil
}
