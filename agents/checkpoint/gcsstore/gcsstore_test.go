/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/storetest"
)

// fakeBackend is a hand-rolled in-memory objectBackend that reproduces GCS
// generation semantics (every write advances the generation; delete is
// generation-conditional) without fake-gcs-server.
type fakeBackend struct {
	mu   sync.Mutex
	gen  int64
	objs map[string]fakeObject
}

type fakeObject struct {
	data []byte
	gen  int64
}

func newFakeBackend() *fakeBackend { return &fakeBackend{objs: map[string]fakeObject{}} }

var _ objectBackend = (*fakeBackend)(nil)

func (f *fakeBackend) write(_ context.Context, name string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen++
	f.objs[name] = fakeObject{data: append([]byte(nil), data...), gen: f.gen}
	return nil
}

func (f *fakeBackend) read(_ context.Context, name string) ([]byte, int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[name]
	if !ok {
		return nil, 0, false, nil
	}
	return append([]byte(nil), o.data...), o.gen, true, nil
}

func (f *fakeBackend) deleteIfGen(_ context.Context, name string, gen int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[name]
	if !ok || o.gen != gen {
		return checkpoint.ErrTokenMismatch
	}
	delete(f.objs, name)
	return nil
}

func newTestStore(identity string, sealer Sealer) (*Store, *fakeBackend) {
	fb := newFakeBackend()
	return &Store{identity: identity, sealer: sealer, backend: fb}, fb
}

func sampleEnvelope(runID string) *checkpoint.Envelope {
	return &checkpoint.Envelope{
		Version:        checkpoint.EnvelopeVersion,
		Provider:       checkpoint.ProviderAnthropic,
		Model:          "claude-fable-5",
		ConfigDigest:   "sha256:deadbeef",
		ReconcilerKey:  "org/repo#42",
		RunID:          runID,
		Turn:           3,
		RemainingTurns: 9,
		PendingToolCalls: []checkpoint.PendingToolCall{{
			ID:        "toolu_01ABC",
			Name:      "ask_a_friend",
			InputJSON: json.RawMessage(`{"question":"proceed?"}`),
		}},
		ProviderState: json.RawMessage(`{"max_tokens":1024}`),
	}
}

// TestConformance asserts the shared Store contract — Load-miss, round-trip,
// deep-copy isolation, claim-once Delete, and re-save superseding — against
// the GCS store over the generation-faithful fake backend.
func TestConformance(t *testing.T) {
	storetest.RunConformance(t, func() checkpoint.Store {
		s, _ := newTestStore("agent", IdentitySealer{})
		return s
	})
}

// TestSaveOverwrites pins the Save contract: a second Save for the same key
// replaces the parked envelope rather than preserving the first.
func TestSaveOverwrites(t *testing.T) {
	s, _ := newTestStore("agent", IdentitySealer{})

	if err := s.Save(t.Context(), "k", sampleEnvelope("run-1")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(t.Context(), "k", sampleEnvelope("run-2")); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, _, ok, err := s.Load(t.Context(), "k")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.RunID != "run-2" {
		t.Errorf("RunID: got = %q, want = %q (re-save must supersede)", got.RunID, "run-2")
	}
}

func TestInvalidKeyRejected(t *testing.T) {
	s, _ := newTestStore("agent", IdentitySealer{})
	for _, key := range []string{"/abs", "a/../b", ".."} {
		t.Run(key, func(t *testing.T) {
			if err := s.Save(t.Context(), key, sampleEnvelope("run-1")); err == nil {
				t.Errorf("Save(%q): got = nil, want error", key)
			}
			if _, _, _, err := s.Load(t.Context(), key); err == nil {
				t.Errorf("Load(%q): got = nil, want error", key)
			}
			if err := s.Delete(t.Context(), key, checkpoint.Token{Generation: 1}); err == nil {
				t.Errorf("Delete(%q): got = nil, want error", key)
			}
		})
	}
}

// deleteRecordingBackend counts deleteIfGen calls so a test can prove Delete
// failed closed without reaching the backend at all.
type deleteRecordingBackend struct {
	objectBackend
	deletes int
}

func (r *deleteRecordingBackend) deleteIfGen(ctx context.Context, name string, gen int64) error {
	r.deletes++
	return r.objectBackend.deleteIfGen(ctx, name, gen)
}

// TestNonPositiveTokenDeleteFailsClosed pins that Delete with a zero or
// negative Token returns ErrTokenMismatch without a backend call: on the real
// backend, GenerationMatch:0 would reach the GCS client as an all-zero
// Conditions struct, which fails with a plain error rather than a
// precondition failure — diverging from the fake backend's behavior.
func TestNonPositiveTokenDeleteFailsClosed(t *testing.T) {
	rb := &deleteRecordingBackend{objectBackend: newFakeBackend()}
	s := &Store{identity: "agent", sealer: IdentitySealer{}, backend: rb}

	if err := s.Save(t.Context(), "k", sampleEnvelope("run-1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, gen := range []int64{0, -1} {
		if err := s.Delete(t.Context(), "k", checkpoint.Token{Generation: gen}); !errors.Is(err, checkpoint.ErrTokenMismatch) {
			t.Errorf("Delete(gen=%d): got = %v, want = ErrTokenMismatch", gen, err)
		}
	}
	if rb.deletes != 0 {
		t.Errorf("backend deleteIfGen calls: got = %d, want = 0", rb.deletes)
	}
	if _, _, ok, err := s.Load(t.Context(), "k"); err != nil || !ok {
		t.Errorf("Load after failed Delete: ok=%v err=%v, want the envelope intact", ok, err)
	}
}

// countingSealer proves the seal/open path is actually exercised and the object
// bytes on the backend are the sealed form, not the plaintext envelope.
type countingSealer struct{ seals, opens int }

func (c *countingSealer) Seal(b []byte) ([]byte, error) {
	c.seals++
	return append([]byte("SEALED:"), b...), nil
}

func (c *countingSealer) Open(b []byte) ([]byte, error) {
	c.opens++
	return b[len("SEALED:"):], nil
}

func TestSealerAppliedToStoredBytes(t *testing.T) {
	cs := &countingSealer{}
	s, fb := newTestStore("agent", cs)
	if err := s.Save(t.Context(), "k", sampleEnvelope("run-1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _, ok, err := fb.read(t.Context(), "agent/k")
	if err != nil || !ok {
		t.Fatalf("backend read: ok=%v err=%v", ok, err)
	}
	if !strings.HasPrefix(string(raw), "SEALED:") {
		t.Errorf("stored bytes not sealed: %q", raw[:min(20, len(raw))])
	}
	if _, _, _, err := s.Load(t.Context(), "k"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cs.seals != 1 || cs.opens != 1 {
		t.Errorf("sealer calls: got = %d seals / %d opens, want = 1/1", cs.seals, cs.opens)
	}
}

// failingSealer returns a fixed error from both operations.
type failingSealer struct{ err error }

func (f failingSealer) Seal([]byte) ([]byte, error) { return nil, f.err }
func (f failingSealer) Open([]byte) ([]byte, error) { return nil, f.err }

func TestSealerErrorsSurface(t *testing.T) {
	sentinel := errors.New("kms unavailable")

	s, _ := newTestStore("agent", failingSealer{err: sentinel})
	if err := s.Save(t.Context(), "k", sampleEnvelope("run-1")); !errors.Is(err, sentinel) {
		t.Errorf("Save with failing Seal: got = %v, want wrapped %v", err, sentinel)
	}

	// Park an envelope with a working sealer, then fail on Open.
	good, fb := newTestStore("agent", IdentitySealer{})
	if err := good.Save(t.Context(), "k", sampleEnvelope("run-1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, _, _, err := (&Store{identity: "agent", sealer: failingSealer{err: sentinel}, backend: fb}).Load(t.Context(), "k"); !errors.Is(err, sentinel) {
		t.Errorf("Load with failing Open: got = %v, want wrapped %v", err, sentinel)
	}
}

func TestObjectNamePrefix(t *testing.T) {
	s, _ := newTestStore("my-agent", IdentitySealer{})
	name, err := s.objectName("org/repo#42")
	if err != nil {
		t.Fatalf("objectName: %v", err)
	}
	if got, want := name, "my-agent/org/repo#42"; got != want {
		t.Errorf("objectName: got = %q, want = %q", got, want)
	}
}
