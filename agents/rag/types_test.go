/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package rag

import (
	"testing"
)

func TestSearchOptionsDefaults(t *testing.T) {
	tests := []struct {
		name  string
		input SearchOptions
		want  SearchOptions
	}{
		{
			name:  "zero values: TopK defaults, no threshold filtering",
			input: SearchOptions{},
			want:  SearchOptions{TopK: DefaultTopK, DistanceThreshold: 0},
		},
		{
			name:  "explicit values preserved",
			input: SearchOptions{TopK: 10, DistanceThreshold: 0.5},
			want:  SearchOptions{TopK: 10, DistanceThreshold: 0.5},
		},
		{
			name:  "negative TopK gets default",
			input: SearchOptions{TopK: -1, DistanceThreshold: 0.5},
			want:  SearchOptions{TopK: DefaultTopK, DistanceThreshold: 0.5},
		},
		{
			name:  "zero DistanceThreshold means no filtering",
			input: SearchOptions{TopK: 3, DistanceThreshold: 0},
			want:  SearchOptions{TopK: 3, DistanceThreshold: 0},
		},
		{
			name:  "explicit threshold preserved",
			input: SearchOptions{TopK: 5, DistanceThreshold: 0.3},
			want:  SearchOptions{TopK: 5, DistanceThreshold: 0.3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.input.defaults()
			if got.TopK != tt.want.TopK {
				t.Errorf("TopK: got = %d, want = %d", got.TopK, tt.want.TopK)
			}
			if got.DistanceThreshold != tt.want.DistanceThreshold {
				t.Errorf("DistanceThreshold: got = %f, want = %f", got.DistanceThreshold, tt.want.DistanceThreshold)
			}
		})
	}
}

func TestSearchOptionsDefaultsDoesNotMutateOriginal(t *testing.T) {
	original := SearchOptions{}
	_ = original.defaults()

	if original.TopK != 0 {
		t.Errorf("original.TopK mutated: got = %d, want = 0", original.TopK)
	}
	if original.DistanceThreshold != 0 {
		t.Errorf("original.DistanceThreshold mutated: got = %f, want = 0", original.DistanceThreshold)
	}
}

func TestWithRestrictsAppliesToConfig(t *testing.T) {
	r := map[string][]string{"domain": {"package-build"}, "lang": {"go"}}
	cfg := resolveUpsertOptions([]UpsertOption{WithRestricts(r)})

	if cfg.restricts == nil {
		t.Fatal("restricts: got = nil, want = non-nil")
	}
	if got, want := len(cfg.restricts), 2; got != want {
		t.Errorf("namespace count: got = %d, want = %d", got, want)
	}
	if got, want := cfg.restricts["domain"][0], "package-build"; got != want {
		t.Errorf("domain allow list: got = %q, want = %q", got, want)
	}
}

func TestResolveUpsertOptionsLastWriteWins(t *testing.T) {
	// When the same option is supplied twice, the later one overrides —
	// keeps the API predictable and avoids merging surprises.
	first := map[string][]string{"domain": {"package-build"}}
	second := map[string][]string{"domain": {"mono"}}

	cfg := resolveUpsertOptions([]UpsertOption{
		WithRestricts(first),
		WithRestricts(second),
	})

	if got, want := cfg.restricts["domain"][0], "mono"; got != want {
		t.Errorf("last WithRestricts domain: got = %q, want = %q", got, want)
	}
}

func TestResolveUpsertOptionsZeroOptions(t *testing.T) {
	cfg := resolveUpsertOptions(nil)
	if cfg.restricts != nil {
		t.Errorf("restricts with no options: got = %v, want = nil", cfg.restricts)
	}
}

func TestRestrictsToProtoEmptyReturnsNil(t *testing.T) {
	// Sending an empty Restrictions slice to the API would behave the same
	// as nil but waste a bit of wire — confirm we omit the field entirely.
	if got := restrictsToProto(nil); got != nil {
		t.Errorf("nil input: got = %v, want = nil", got)
	}
	if got := restrictsToProto(map[string][]string{}); got != nil {
		t.Errorf("empty map input: got = %v, want = nil", got)
	}
}

func TestRestrictsToProtoStableOrdering(t *testing.T) {
	// Stable wire ordering makes payloads diff-friendly in logs and
	// catches regressions in any equality testing the caller does.
	in := map[string][]string{"zeta": {"z"}, "alpha": {"a"}, "mu": {"m"}}
	got := restrictsToProto(in)

	if len(got) != 3 {
		t.Fatalf("namespace count: got = %d, want = 3", len(got))
	}
	want := []string{"alpha", "mu", "zeta"}
	for i, ns := range want {
		if got[i].GetNamespace() != ns {
			t.Errorf("namespace[%d]: got = %q, want = %q", i, got[i].GetNamespace(), ns)
		}
	}
}
