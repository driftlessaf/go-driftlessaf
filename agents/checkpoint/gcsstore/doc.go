/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package gcsstore implements checkpoint.Store over a Google Cloud Storage
// bucket, for durably parking suspended agent envelopes across the human-wait.
//
// # Object layout
//
// Exactly one object is parked per {identity}/{key}: the object name is
// "{identity}/{key}", mirroring gcsstatusmanager. The Envelope's RunID is a
// field of the stored object, NOT part of the path, so Load(key) is never
// ambiguous and a re-suspend of the same key reuses the same object name.
//
// # CAS semantics
//
//   - Save writes unconditionally, matching the checkpoint.Store contract: a
//     re-save for a key replaces the parked envelope (one pending suspension
//     per key, last save supersedes) and GCS advances the object generation,
//     so any Token loaded before the re-save can no longer claim the object.
//     First-writer-wins (Conditions{DoesNotExist}) was deliberately not used:
//     it would strand a superseding suspension behind a stale envelope until
//     something cleared the object, while unconditional writes keep Save free
//     of ordering concerns — claim-once ordering is expressed with Load +
//     Delete, never with Save.
//   - Load returns the object's GCS generation as the checkpoint.Token.
//   - Delete uses Conditions{GenerationMatch:Token.Generation} as the claim: a
//     generation mismatch or an already-deleted object surfaces as
//     checkpoint.ErrTokenMismatch, so exactly one waker wins the claim race.
//
// # Sealing
//
// Every byte passes through a caller-supplied Sealer on its way to and from
// the bucket, so the store never knows whether it holds plaintext or
// ciphertext and the KMS/AEAD SDK is never pulled into this package:
//
//	                    SAVE (park)
//	checkpoint.Envelope ──json.Marshal──► plaintext bytes
//	                                            │
//	                                            ▼
//	                                  ┌──────────────────┐
//	                                  │  Sealer.Seal(b)  │
//	                                  └──────────────────┘
//	                                            │  sealed bytes
//	                                            ▼
//	                            GCS object {identity}/{key}
//	                            (generation N = the CAS Token)
//	                                            │
//	                    LOAD (wake)             ▼
//	                                  ┌──────────────────┐
//	                                  │  Sealer.Open(b)  │
//	                                  └──────────────────┘
//	                                            │  plaintext or error
//	                                            ▼
//	                    json.Unmarshal ──► checkpoint.Envelope
//
// This placement gives two properties. First, CAS is independent of crypto:
// the claim (Delete with GenerationMatch) conditions on the object, not its
// contents, so exactly-once wake semantics are identical for plaintext and
// ciphertext. Second, Sealer is a two-method local interface, so consumers
// that do not want KMS never compile against it.
//
// IdentitySealer passes bytes through unchanged, storing the envelope as
// readable JSON — the right choice for dev and tests, and what makes
// "gcloud storage cat | jq" inspection of a parked run possible. Production
// deployments should instead wire a KMS envelope Sealer:
//
//	SEAL (per checkpoint write)                     Cloud KMS
//	───────────────────────────                     ─────────
//	1. generate random DEK ────────── KMS.Encrypt ──► KEK (never leaves KMS,
//	   (AES-256-GCM data key)  ◄── wrapped DEK ────   rotated, IAM-gated)
//	2. encrypt envelope with DEK
//	3. store one blob:
//	   { wrapped_dek, iv, ciphertext, kek } ──► GCS object
//
//	OPEN (per wake)
//	───────────────
//	1. read blob ──► KMS.Decrypt(wrapped_dek) ──► DEK
//	2. DEK decrypts and AUTHENTICATES the ciphertext: tampered
//	   stored bytes are a hard Open failure, not a poisoned
//	   conversation that silently resumes.
//
// Envelope encryption keeps KMS traffic per-key-wrap rather than
// per-megabyte: the potentially large transcript is encrypted locally with
// the DEK, and KMS only ever touches the small wrapped key. With such a
// Sealer, reading a checkpoint's plaintext requires both bucket read access
// and KMS decrypt on the KEK, and the AEAD authentication tag makes parked
// transcripts tamper-evident against anyone holding only bucket write
// access. The bucket should still be CMEK-encrypted at rest independently.
//
// # Deployment (Terraform, not in this package)
//
// This package writes no Terraform. Deployers should provision a dedicated
// bucket via the standard bucket module with an object lifecycle rule of
// lifecycle_age_days = 14 (envelopes are fail-closed past Envelope.Deadline, so
// a 14-day floor bounds orphaned checkpoints), and pass the bucket name to the
// reconciler through a CHECKPOINT_BUCKET environment variable.
//
// Conformance with the Store contract is asserted by the shared storetest
// suite, run against an in-memory backend that reproduces GCS generation and
// precondition semantics.
package gcsstore
