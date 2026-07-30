/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package kmsseal is the production [gcsstore.Sealer]: envelope encryption
// with a Cloud KMS-wrapped data key, exactly the SEAL/OPEN shape the gcsstore
// package doc prescribes.
//
// Each Seal generates a fresh random AES-256-GCM DEK, encrypts the payload
// locally, wraps the DEK with a KMS symmetric KEK (which never leaves KMS,
// rotates on its own schedule, and is IAM-gated per direction: Encrypt to
// seal, Decrypt to open), and stores everything as one self-contained JSON
// blob:
//
//	{ "kek", "wrapped_dek", "nonce", "ciphertext" }
//
// Reading a sealed payload therefore requires BOTH storage read access and
// KMS Decrypt on the KEK, and the GCM authentication tag makes stored blobs
// tamper-evident against anyone holding only storage write access: Open of a
// MODIFIED blob is a hard failure. The KMS wrap binds a package-constant
// AAD, so DEKs wrapped here cannot be unwrapped through an unrelated caller
// of the same KEK.
//
// One tampering class the Sealer cannot see: wholesale substitution. Two
// valid blobs sealed under the same KEK are interchangeable at this layer —
// nothing binds a blob to the object it is stored as — so a writer can
// replace object A's blob with object B's and Open succeeds. Callers for
// whom blob/object binding matters must embed their own context (the object
// key, an ID) inside the sealed payload and verify it after Open.
//
// The Sealer seals anything handed to it; gcsstore uses it for checkpoint
// envelopes, and it equally suits any small secret-bearing object that needs
// the two-factor read property (e.g. a microVM suspend reference bundle).
package kmsseal
