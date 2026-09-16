/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-cmp/cmp"
)

// stubAnalyzer records the paths and prior it received and returns fixed
// diagnostics or an error.
type stubAnalyzer struct {
	diags []Diagnostic
	err   error
	paths []string
	prior []Diagnostic
}

func (s *stubAnalyzer) Analyze(_ context.Context, _ *gogit.Worktree, paths []string, prior ...Diagnostic) ([]Diagnostic, error) {
	s.paths, s.prior = paths, prior
	return s.diags, s.err
}

func TestSequence(t *testing.T) {
	t.Parallel()
	first := Diagnostic{Path: "a.go", Line: 1, Rule: "first", Message: "one"}
	second := Diagnostic{Path: "b.go", Line: 2, Rule: "second", Message: "two"}
	third := Diagnostic{Path: "c.go", Line: 3, Rule: "second", Message: "three"}
	prior := []Diagnostic{{Path: "p.go", Rule: "prior"}}
	paths := []string{"a.go", "b.go"}

	tests := []struct {
		name      string
		analyzers []*stubAnalyzer
		want      []Diagnostic
		wantErr   string
	}{
		{
			name:      "no analyzers yields no diagnostics",
			analyzers: nil,
			want:      nil,
		},
		{
			name:      "diagnostics concatenate in analyzer order",
			analyzers: []*stubAnalyzer{{diags: []Diagnostic{first}}, {diags: []Diagnostic{second, third}}},
			want:      []Diagnostic{first, second, third},
		},
		{
			name:      "an empty analyzer contributes nothing",
			analyzers: []*stubAnalyzer{{}, {diags: []Diagnostic{second}}},
			want:      []Diagnostic{second},
		},
		{
			name:      "the first error stops the sequence and names the analyzer",
			analyzers: []*stubAnalyzer{{diags: []Diagnostic{first}}, {err: errors.New("boom")}, {diags: []Diagnostic{third}}},
			wantErr:   "analyzer *metapathreconciler.stubAnalyzer: boom",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			analyzers := make([]Analyzer, 0, len(tc.analyzers))
			for _, a := range tc.analyzers {
				analyzers = append(analyzers, a)
			}
			got, err := Sequence(analyzers...).Analyze(t.Context(), nil, paths, prior...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, want containing %q", err, tc.wantErr)
				}
				if tc.analyzers[2].paths != nil {
					t.Error("analyzer after the failure ran, want the sequence to stop")
				}
				return
			}
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("diagnostics (-want, +got):\n%s", diff)
			}
			for i, a := range tc.analyzers {
				if diff := cmp.Diff(paths, a.paths); diff != "" {
					t.Errorf("analyzer %d paths (-want, +got):\n%s", i, diff)
				}
				if diff := cmp.Diff(prior, a.prior); diff != "" {
					t.Errorf("analyzer %d prior (-want, +got):\n%s", i, diff)
				}
			}
		})
	}
}
