/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package payloadcrypt seals the sensitive free-text fields of an agent trace
// (prompt, completion, tool params/results, reasoning) before they leave the
// emitting process, so the CloudEvent — and the BigQuery row the recorder
// derives from it — carries ciphertext rather than plaintext.
//
// The scheme is envelope encryption: a fresh AES-256 data-encryption key (DEK)
// is minted per CloudEvent, the DEK is wrapped by a Cloud KMS symmetric KEK
// (one KMS Encrypt call per event, regardless of how many fields it seals), and
// each field is AES-256-GCM-sealed under the DEK with a fresh nonce. Recovering
// any field requires KMS Decrypt on the KEK (the break-glass second factor,
// PAM-gated) followed by an AES-GCM open — so a reader holding only BigQuery
// dataViewer sees ciphertext and nothing more.
//
// The producer side holds encrypt-only (roles/cloudkms.cryptoKeyEncrypter) on
// the KEK and can never read a payload back. Reader/break-glass code uses Open
// with an injected unwrap callback, mirroring the GCP-free split in
// public/sdk/uploads/envelope.go. The Cloud KMS-backed wrap for the producer
// side lives in the sibling kmsseal package.
package payloadcrypt
