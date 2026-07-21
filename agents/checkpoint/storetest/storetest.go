/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"github.com/google/go-cmp/cmp"
)

// sampleEnvelope returns a representative envelope with every field populated so
// the round-trip assertions exercise the raw-JSON and slice carriers.
func sampleEnvelope(runID string) *checkpoint.Envelope {
	return &checkpoint.Envelope{
		Version:        checkpoint.EnvelopeVersion,
		Provider:       "anthropic",
		Model:          "claude-fable-5",
		SDKVersion:     "v1.51.1",
		ConfigDigest:   "sha256:deadbeef",
		ReconcilerKey:  "org/repo#42",
		RunID:          runID,
		Turn:           3,
		RemainingTurns: 9,
		Reason:         "ask_a_friend",
		PendingToolCalls: []checkpoint.PendingToolCall{{
			ID:        "toolu_01ABC",
			Name:      "ask_a_friend",
			InputJSON: json.RawMessage(`{"question":"proceed?"}`),
		}},
		ProviderState: json.RawMessage(`{"model":"claude-fable-5","max_tokens":1024}`),
		LoopState:     json.RawMessage(`{"turn":3}`),
		TraceID:       "trace-abc",
		Deadline:      time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
	}
}

// RunConformance exercises the Store contract: Load-miss, Save/Load round-trip,
// claim-once Delete, token-mismatch on stale/absent tokens, and re-save
// invalidating an old token. newStore must return a fresh, empty Store on each
// call.
func RunConformance(t *testing.T, newStore func() checkpoint.Store) {
	t.Helper()

	t.Run("LoadMissing", func(t *testing.T) {
		s := newStore()
		env, tok, ok, err := s.Load(t.Context(), "absent")
		if err != nil {
			t.Fatalf("Load: unexpected error: %v", err)
		}
		if ok {
			t.Fatalf("Load: want ok=false for missing key, got env=%+v tok=%+v", env, tok)
		}
		if env != nil {
			t.Fatalf("Load: want nil envelope for missing key, got %+v", env)
		}
	})

	t.Run("SaveLoadRoundTrip", func(t *testing.T) {
		s := newStore()
		want := sampleEnvelope("run-1")
		if err := s.Save(t.Context(), "k", want); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, tok, ok, err := s.Load(t.Context(), "k")
		if err != nil || !ok {
			t.Fatalf("Load: ok=%v err=%v", ok, err)
		}
		if tok == (checkpoint.Token{}) {
			t.Fatalf("Load: expected a non-zero token")
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("round-trip mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("LoadReturnsDeepCopy", func(t *testing.T) {
		s := newStore()
		orig := sampleEnvelope("run-1")
		if err := s.Save(t.Context(), "k", orig); err != nil {
			t.Fatalf("Save: %v", err)
		}
		// Mutating the loaded copy must not affect a subsequent Load.
		got1, _, _, err := s.Load(t.Context(), "k")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got1.PendingToolCalls[0].ID = "mutated"
		got1.ProviderState = json.RawMessage(`{"mutated":true}`)
		got2, _, _, err := s.Load(t.Context(), "k")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got2.PendingToolCalls[0].ID != "toolu_01ABC" {
			t.Fatalf("store aliased caller-visible slice: %q", got2.PendingToolCalls[0].ID)
		}
		if string(got2.ProviderState) != `{"model":"claude-fable-5","max_tokens":1024}` {
			t.Fatalf("store aliased raw provider state: %s", got2.ProviderState)
		}
	})

	t.Run("SaveTakesDeepCopy", func(t *testing.T) {
		s := newStore()
		orig := sampleEnvelope("run-1")
		if err := s.Save(t.Context(), "k", orig); err != nil {
			t.Fatalf("Save: %v", err)
		}
		// Mutating the envelope handed to Save must not affect stored state.
		// The raw-JSON bytes are corrupted in place so that even a shallow
		// struct copy sharing the backing array is caught.
		orig.PendingToolCalls[0].ID = "mutated"
		orig.ProviderState[0] = 'X'
		got, _, _, err := s.Load(t.Context(), "k")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.PendingToolCalls[0].ID != "toolu_01ABC" {
			t.Fatalf("store aliased the saved slice: %q", got.PendingToolCalls[0].ID)
		}
		if string(got.ProviderState) != `{"model":"claude-fable-5","max_tokens":1024}` {
			t.Fatalf("store aliased the saved raw provider state: %s", got.ProviderState)
		}
	})

	t.Run("ClaimOnce", func(t *testing.T) {
		s := newStore()
		if err := s.Save(t.Context(), "k", sampleEnvelope("run-1")); err != nil {
			t.Fatalf("Save: %v", err)
		}
		_, tok, ok, err := s.Load(t.Context(), "k")
		if err != nil || !ok {
			t.Fatalf("Load: ok=%v err=%v", ok, err)
		}
		// First delete with the valid token wins.
		if err := s.Delete(t.Context(), "k", tok); err != nil {
			t.Fatalf("first Delete: %v", err)
		}
		// A second delete with the same (now stale) token loses the claim.
		if err := s.Delete(t.Context(), "k", tok); !errors.Is(err, checkpoint.ErrTokenMismatch) {
			t.Fatalf("second Delete: want ErrTokenMismatch, got %v", err)
		}
		// The key is gone.
		_, _, ok, err = s.Load(t.Context(), "k")
		if err != nil {
			t.Fatalf("Load after delete: %v", err)
		}
		if ok {
			t.Fatalf("Load after delete: key still present")
		}
	})

	t.Run("DeleteWrongToken", func(t *testing.T) {
		s := newStore()
		if err := s.Save(t.Context(), "k", sampleEnvelope("run-1")); err != nil {
			t.Fatalf("Save: %v", err)
		}
		_, tok, _, err := s.Load(t.Context(), "k")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		wrong := checkpoint.Token{Generation: tok.Generation + 9999}
		if err := s.Delete(t.Context(), "k", wrong); !errors.Is(err, checkpoint.ErrTokenMismatch) {
			t.Fatalf("Delete wrong token: want ErrTokenMismatch, got %v", err)
		}
		// Entry must survive a failed delete.
		if _, _, ok, _ := s.Load(t.Context(), "k"); !ok {
			t.Fatalf("entry vanished after a mismatched Delete")
		}
	})

	t.Run("DeleteAbsentKey", func(t *testing.T) {
		s := newStore()
		if err := s.Delete(t.Context(), "never", checkpoint.Token{Generation: 1}); !errors.Is(err, checkpoint.ErrTokenMismatch) {
			t.Fatalf("Delete absent: want ErrTokenMismatch, got %v", err)
		}
	})

	t.Run("ResaveInvalidatesOldToken", func(t *testing.T) {
		s := newStore()
		if err := s.Save(t.Context(), "k", sampleEnvelope("run-1")); err != nil {
			t.Fatalf("first Save: %v", err)
		}
		_, oldTok, _, err := s.Load(t.Context(), "k")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := s.Save(t.Context(), "k", sampleEnvelope("run-2")); err != nil {
			t.Fatalf("second Save: %v", err)
		}
		// The pre-resave token must no longer claim the object.
		if err := s.Delete(t.Context(), "k", oldTok); !errors.Is(err, checkpoint.ErrTokenMismatch) {
			t.Fatalf("Delete with stale token: want ErrTokenMismatch, got %v", err)
		}
		// The fresh token does.
		_, newTok, _, err := s.Load(t.Context(), "k")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := s.Delete(t.Context(), "k", newTok); err != nil {
			t.Fatalf("Delete with fresh token: %v", err)
		}
	})
}
