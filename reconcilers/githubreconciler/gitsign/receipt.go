/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsign

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/github/smimesign/ietf-cms/protocol"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	siggit "github.com/sigstore/gitsign/pkg/git"
	rekorpb "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/rekor/pkg/types"
	hashedrekord "github.com/sigstore/rekor/pkg/types/hashedrekord/v0.0.1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"google.golang.org/protobuf/proto"
)

// NewDefaultVerifier loads Fulcio certificates and Rekor keys from Sigstore's
// public trusted root. The verifier checks commits offline with that snapshot.
// Construct a new verifier to pick up changes to the trust root.
func NewDefaultVerifier(ctx context.Context) (*Verifier, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tr, err := root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithContext(ctx))
	if err != nil {
		return nil, &TrustRootError{Cause: err}
	}
	return verifierFromRoot(tr)
}

func verifierFromRoot(tr *root.TrustedRoot) (*Verifier, error) {
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	for _, authority := range tr.FulcioCertificateAuthorities() {
		ca, ok := authority.(*root.FulcioCertificateAuthority)
		if !ok {
			return nil, &TrustRootError{Cause: fmt.Errorf("unexpected Fulcio authority %T", authority)}
		}
		roots.AddCert(ca.Root)
		for _, intermediate := range ca.Intermediates {
			intermediates.AddCert(intermediate)
		}
	}
	certs, err := siggit.NewCertVerifier(siggit.WithRootPool(roots), siggit.WithIntermediatePool(intermediates))
	if err != nil {
		return nil, &TrustRootError{Cause: err}
	}
	return NewVerifier(certs, func(ctx context.Context, sig []byte, cert *x509.Certificate) (*models.LogEntryAnon, error) {
		entry, err := embeddedEntry(ctx, sig, cert)
		if err != nil {
			return nil, err
		}
		if err := cosign.VerifyTLogEntryOffline(ctx, entry, nil, tr); err != nil {
			return nil, err
		}
		when := time.Unix(*entry.IntegratedTime, 0)
		if _, err := verify.VerifyLeafCertificate(when, cert, tr); err != nil {
			return nil, fmt.Errorf("verifying Fulcio authority at Rekor integrated time: %w", err)
		}
		return entry, nil
	}), nil
}

var rekorEntryOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 3, 1}

func embeddedEntry(ctx context.Context, sig []byte, cert *x509.Certificate) (*models.LogEntryAnon, error) {
	if block, _ := pem.Decode(sig); block != nil {
		sig = block.Bytes
	}
	cmsMu.Lock()
	info, err := protocol.ParseContentInfo(sig)
	cmsMu.Unlock()
	if err != nil {
		return nil, err
	}
	data, err := info.SignedDataContent()
	if err != nil {
		return nil, err
	}
	if len(data.SignerInfos) == 0 {
		return nil, errors.New("CMS signature has no signer")
	}
	signer := data.SignerInfos[0]
	if _, err := signer.FindCertificate([]*x509.Certificate{cert}); err != nil {
		return nil, fmt.Errorf("matching receipt signer: %w", err)
	}
	attribute, err := signer.UnsignedAttrs.GetOnlyAttributeValueBytes(rekorEntryOID)
	if err != nil {
		return nil, fmt.Errorf("reading Rekor receipt: %w", err)
	}
	var encoded []byte
	rest, err := asn1.Unmarshal(attribute.FullBytes, &encoded)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing data after Rekor receipt")
	}
	var receipt rekorpb.TransparencyLogEntry
	if err := proto.Unmarshal(encoded, &receipt); err != nil {
		return nil, err
	}
	proof := receipt.GetInclusionProof()
	if proof == nil || receipt.LogIndex < 0 || proof.LogIndex < 0 || proof.TreeSize <= proof.LogIndex {
		return nil, errors.New("invalid Rekor inclusion proof or index")
	}
	body, err := receiptBody(ctx, signer, cert)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(proof.Hashes))
	for _, hash := range proof.Hashes {
		hashes = append(hashes, hex.EncodeToString(hash))
	}
	// Rekor signs the global log index. The inclusion proof uses the
	// index within the shard, which can differ.
	return &models.LogEntryAnon{
		Body:           base64.StdEncoding.EncodeToString(body),
		LogID:          new(hex.EncodeToString(receipt.GetLogId().GetKeyId())),
		LogIndex:       &receipt.LogIndex,
		IntegratedTime: &receipt.IntegratedTime,
		Verification: &models.LogEntryAnonVerification{
			SignedEntryTimestamp: receipt.GetInclusionPromise().GetSignedEntryTimestamp(),
			InclusionProof: &models.InclusionProof{
				Checkpoint: new(proof.GetCheckpoint().GetEnvelope()),
				LogIndex:   &proof.LogIndex,
				TreeSize:   &proof.TreeSize,
				RootHash:   new(hex.EncodeToString(proof.RootHash)),
				Hashes:     hashes,
			},
		},
	}, nil
}

// Gitsign omits the Rekor body from the receipt. Reconstruct it from the
// authenticated CMS signer so the inclusion proof covers this signature.
func receiptBody(ctx context.Context, signer protocol.SignerInfo, cert *x509.Certificate) ([]byte, error) {
	message, err := signer.SignedAttrs.MarshaledForVerification()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(message)
	key, err := cryptoutils.MarshalCertificateToPEM(cert)
	if err != nil {
		return nil, err
	}
	return types.CanonicalizeEntry(ctx, &hashedrekord.V001Entry{
		HashedRekordObj: models.HashedrekordV001Schema{
			Data: &models.HashedrekordV001SchemaData{
				Hash: &models.HashedrekordV001SchemaDataHash{
					Algorithm: new(models.HashedrekordV001SchemaDataHashAlgorithmSha256),
					Value:     new(hex.EncodeToString(digest[:])),
				},
			},
			Signature: &models.HashedrekordV001SchemaSignature{
				Content:   signer.Signature,
				PublicKey: &models.HashedrekordV001SchemaSignaturePublicKey{Content: key},
			},
		},
	})
}
