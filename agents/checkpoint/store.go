/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint

import (
	"context"
	"errors"
)

// ErrTokenMismatch is returned by Store.Delete when the supplied Token no
// longer matches the stored object's current CAS handle: the object was
// re-saved, already deleted, or never existed. It is the signal a concurrent
// waker uses to detect it lost a claim race.
var ErrTokenMismatch = errors.New("checkpoint: CAS token mismatch")

// Token is an opaque compare-and-swap handle minted by a Store on Load. A
// caller passes the same Token back to Delete to claim-and-remove an envelope
// atomically; if the object changed in between, Delete fails with
// ErrTokenMismatch. Generation is the store-specific version counter (a GCS
// object generation, or a local monotonic counter). The zero Token is invalid.
type Token struct {
	Generation int64 `json:"generation"`
}

// Store is the durable home for suspended envelopes. Implementations park at
// most one envelope per key (one pending suspension per {identity}/{key}); the
// RunID lives inside the Envelope rather than in the key so Load is never
// ambiguous.
//
// The contract is deliberately small so both a local dev store and an
// AEAD-sealed GCS store satisfy it, and so PR9's orchestration can build
// claim-once wake semantics purely from Load + Delete CAS.
type Store interface {
	// Save durably writes env under key, overwriting any existing envelope and
	// advancing its CAS generation. Save is unconditional: claim-once ordering
	// is expressed with Load + Delete, not with Save.
	Save(ctx context.Context, key string, env *Envelope) error

	// Load returns the envelope stored under key together with its current CAS
	// Token. The bool is false (with a nil envelope, zero Token, and nil error)
	// when no envelope exists for key.
	Load(ctx context.Context, key string) (*Envelope, Token, bool, error)

	// Delete removes the envelope under key only if tok still matches the
	// stored object's current generation, returning ErrTokenMismatch otherwise
	// (including when the key is already gone). This is the claim primitive:
	// exactly one caller holding a given Token wins the delete.
	Delete(ctx context.Context, key string, tok Token) error
}
