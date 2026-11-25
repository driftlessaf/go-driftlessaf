/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dagtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/dag"
)

type key struct{ step, hash string }

// Mem is an in-memory dag.Resumer. It is safe for concurrent use.
type Mem struct {
	mu      sync.Mutex
	outputs map[key][]byte
}

var _ dag.Resumer = (*Mem)(nil)

// NewMem returns an empty Mem.
func NewMem() *Mem { return &Mem{outputs: map[key][]byte{}} }

// Lookup returns the output saved for step under hash.
func (m *Mem) Lookup(_ context.Context, step, hash string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, ok := m.outputs[key{step, hash}]
	return slices.Clone(out), ok, nil
}

// Save stores a copy of output for step under hash, replacing any earlier one.
func (m *Mem) Save(_ context.Context, step, hash string, output []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outputs[key{step, hash}] = append([]byte{}, output...)
	return nil
}

// RunResumerConformance holds a dag.Resumer to the Lookup and Save contract.
// newResumer must return a fresh, empty Resumer on each call.
func RunResumerConformance(t *testing.T, newResumer func(t *testing.T) dag.Resumer) {
	type save struct {
		step, hash string
		output     []byte
	}
	type lookup struct {
		step, hash string
		want       []byte // nil means a miss
	}
	out, newer := []byte(rand.Text()), []byte(rand.Text())
	hash := rand.Text()

	tests := []struct {
		name    string
		saves   []save
		lookups []lookup
	}{{
		name:    "empty store misses",
		lookups: []lookup{{step: "a", hash: hash}},
	}, {
		name:    "exact step and hash hit",
		saves:   []save{{"critique/model-a", hash, out}},
		lookups: []lookup{{step: "critique/model-a", hash: hash, want: out}},
	}, {
		name:    "a different hash misses",
		saves:   []save{{"a", hash, out}},
		lookups: []lookup{{step: "a", hash: rand.Text()}},
	}, {
		name:    "a different step misses",
		saves:   []save{{"critique/model-a", hash, out}},
		lookups: []lookup{{step: "critique/model-b", hash: hash}},
	}, {
		name:    "an empty output is found",
		saves:   []save{{"a", hash, []byte{}}},
		lookups: []lookup{{step: "a", hash: hash, want: []byte{}}},
	}, {
		name:    "a later save replaces an earlier one",
		saves:   []save{{"a", hash, out}, {"a", hash, newer}},
		lookups: []lookup{{step: "a", hash: hash, want: newer}},
	}, {
		name:  "step and hash do not alias",
		saves: []save{{"a/b", "c", []byte("a/b c")}, {"a", "b/c", []byte("a b/c")}},
		lookups: []lookup{
			{step: "a/b", hash: "c", want: []byte("a/b c")},
			{step: "a", hash: "b/c", want: []byte("a b/c")},
			{step: "a/b/c", hash: ""},
		},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newResumer(t)
			for _, s := range tc.saves {
				if err := r.Save(t.Context(), s.step, s.hash, s.output); err != nil {
					t.Fatalf("Save(%q, %q): %v", s.step, s.hash, err)
				}
			}
			for _, l := range tc.lookups {
				got, found, err := r.Lookup(t.Context(), l.step, l.hash)
				switch {
				case err != nil:
					t.Errorf("Lookup(%q, %q): %v", l.step, l.hash, err)
				case l.want == nil && found:
					t.Errorf("Lookup(%q, %q): got a hit %q, want a miss", l.step, l.hash, got)
				case l.want != nil && !found:
					t.Errorf("Lookup(%q, %q): got a miss, want %q", l.step, l.hash, l.want)
				case l.want != nil && !bytes.Equal(got, l.want):
					t.Errorf("Lookup(%q, %q): got = %q, want = %q", l.step, l.hash, got, l.want)
				}
			}
		})
	}

	t.Run("saved output is not aliased to the caller's slice", func(t *testing.T) {
		r := newResumer(t)
		buf := []byte(rand.Text())
		want := bytes.Clone(buf)
		if err := r.Save(t.Context(), "a", hash, buf); err != nil {
			t.Fatalf("Save: %v", err)
		}
		buf[0] ^= 0xff
		got, _, err := r.Lookup(t.Context(), "a", hash)
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Lookup after mutating the saved slice: got = %q, want = %q", got, want)
		}
	})

	t.Run("concurrent saves and lookups", func(t *testing.T) {
		r := newResumer(t)
		var wg sync.WaitGroup
		t.Cleanup(wg.Wait)
		for i := range 8 {
			step := fmt.Sprint("step-", i)
			wg.Go(func() {
				if err := r.Save(t.Context(), step, hash, out); err != nil {
					t.Errorf("Save(%q): %v", step, err)
					return
				}
				if got, found, err := r.Lookup(t.Context(), step, hash); err != nil || !found || !bytes.Equal(got, out) {
					t.Errorf("Lookup(%q): got = %q, found = %v, err = %v, want = %q", step, got, found, err, out)
				}
			})
		}
		wg.Wait()
	})
}
