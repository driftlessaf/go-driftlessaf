/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package memstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/memstore"
)

// ExampleNew demonstrates the Save/Load/Delete round-trip and the claim-once
// CAS semantics of the in-memory store.
func ExampleNew() {
	ctx := context.Background()
	store := memstore.New()

	env := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		Provider:      checkpoint.ProviderAnthropic,
		ReconcilerKey: "org/repo#42",
		RunID:         "run-1",
		ProviderState: json.RawMessage(`{"model":"claude-fable-5"}`),
	}
	if err := store.Save(ctx, env.ReconcilerKey, env); err != nil {
		fmt.Println("error:", err)
		return
	}

	loaded, tok, ok, err := store.Load(ctx, env.ReconcilerKey)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("parked:", ok, loaded.RunID)

	// Delete with the Load token claims the envelope exactly once; a stale
	// token loses with ErrTokenMismatch.
	fmt.Println("claimed:", store.Delete(ctx, env.ReconcilerKey, tok))
	fmt.Println("stale claim:",
		errors.Is(store.Delete(ctx, env.ReconcilerKey, tok), checkpoint.ErrTokenMismatch))
	// Output:
	// parked: true run-1
	// claimed: <nil>
	// stale claim: true
}
