/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package kmsseal

import (
	"context"
	"errors"
	"fmt"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"

	"chainguard.dev/driftlessaf/agents/agenttrace/payloadcrypt"
)

// New returns a payloadcrypt.Encryptor that wraps per-event DEKs via a Cloud KMS
// symmetric Encrypt against keyName
// (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>). The wrap binds
// payloadcrypt.DEKWrapAAD as additional authenticated data. The returned close
// func releases the KMS client; call it at process shutdown.
func New(ctx context.Context, keyName string) (enc *payloadcrypt.Encryptor, closeFn func() error, err error) {
	if keyName == "" {
		return nil, nil, errors.New("kmsseal: keyName is required")
	}
	client, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("kmsseal: building KMS client: %w", err)
	}
	aad := payloadcrypt.DEKWrapAAD()
	wrap := func(ctx context.Context, dek []byte) ([]byte, error) {
		resp, err := client.Encrypt(ctx, &kmspb.EncryptRequest{
			Name:                        keyName,
			Plaintext:                   dek,
			AdditionalAuthenticatedData: aad,
		})
		if err != nil {
			return nil, fmt.Errorf("kmsseal: wrap DEK: %w", err)
		}
		return resp.Ciphertext, nil
	}
	e, err := payloadcrypt.New(keyName, wrap)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return e, client.Close, nil
}
