/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package jsonlstore_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/jsonlstore"
	"chainguard.dev/driftlessaf/agents/checkpoint/storetest"
	"github.com/google/go-cmp/cmp"
)

func TestConformance(t *testing.T) {
	// Each newStore call gets its own fresh log file.
	storetest.RunConformance(t, func() checkpoint.Store {
		path := filepath.Join(t.TempDir(), "checkpoints.jsonl")
		s, err := jsonlstore.New(path)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return s
	})
}

// TestReplaySurvivesReopen proves the append-only log rebuilds live state
// (including a tombstone and the CAS counter) across a fresh open.
func TestReplaySurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.jsonl")
	env := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		ReconcilerKey: "org/repo#7",
		RunID:         "run-7",
		ProviderState: json.RawMessage(`{"model":"m"}`),
	}

	s1, err := jsonlstore.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()
	if err := s1.Save(ctx, "live", env); err != nil {
		t.Fatalf("Save live: %v", err)
	}
	if err := s1.Save(ctx, "gone", env); err != nil {
		t.Fatalf("Save gone: %v", err)
	}
	_, tok, _, err := s1.Load(ctx, "gone")
	if err != nil {
		t.Fatalf("Load gone: %v", err)
	}
	if err := s1.Delete(ctx, "gone", tok); err != nil {
		t.Fatalf("Delete gone: %v", err)
	}

	// Reopen: the tombstoned key must be gone, the live key must round-trip,
	// and a fresh Save must mint a strictly higher generation than any replayed.
	s2, err := jsonlstore.New(path)
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	if _, _, ok, _ := s2.Load(ctx, "gone"); ok {
		t.Fatalf("tombstoned key resurrected after reopen")
	}
	got, liveTok, ok, err := s2.Load(ctx, "live")
	if err != nil || !ok {
		t.Fatalf("Load live after reopen: ok=%v err=%v", ok, err)
	}
	if diff := cmp.Diff(env, got); diff != "" {
		t.Fatalf("live envelope changed across reopen (-want +got):\n%s", diff)
	}
	// The old generation for "live" from before reopen was 1; after a re-save it
	// must advance beyond the replayed counter (which had reached 3).
	if err := s2.Save(ctx, "live", env); err != nil {
		t.Fatalf("Save after reopen: %v", err)
	}
	_, newTok, _, _ := s2.Load(ctx, "live")
	if newTok.Generation <= liveTok.Generation {
		t.Fatalf("generation did not advance across reopen: old=%d new=%d", liveTok.Generation, newTok.Generation)
	}
}

// TestReplaySurvivesOversizedRecord proves one very large envelope — a
// ProviderState beyond the 16MB a fixed-max-token scanner could read — does
// not make the store unopenable on restart, locking out every other parked
// checkpoint.
func TestReplaySurvivesOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.jsonl")
	ctx := t.Context()

	s1, err := jsonlstore.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	big := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		ReconcilerKey: "org/repo#8",
		RunID:         "run-8",
		ProviderState: json.RawMessage(`"` + strings.Repeat("x", 17*1024*1024) + `"`),
	}
	if err := s1.Save(ctx, "big", big); err != nil {
		t.Fatalf("Save big: %v", err)
	}
	small := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		ReconcilerKey: "org/repo#9",
		RunID:         "run-9",
		ProviderState: json.RawMessage(`{"model":"m"}`),
	}
	if err := s1.Save(ctx, "small", small); err != nil {
		t.Fatalf("Save small: %v", err)
	}

	s2, err := jsonlstore.New(path)
	if err != nil {
		t.Fatalf("reopen with oversized record: %v", err)
	}
	got, _, ok, err := s2.Load(ctx, "big")
	if err != nil || !ok {
		t.Fatalf("Load big after reopen: ok=%v err=%v", ok, err)
	}
	if len(got.ProviderState) != len(big.ProviderState) {
		t.Fatalf("oversized ProviderState changed across reopen: got %d bytes, want %d",
			len(got.ProviderState), len(big.ProviderState))
	}
	if _, _, ok, err := s2.Load(ctx, "small"); err != nil || !ok {
		t.Fatalf("Load small after reopen: ok=%v err=%v", ok, err)
	}
}
