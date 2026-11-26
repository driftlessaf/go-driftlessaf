/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// fixedAnalyzer returns fixed diagnostics and a fixed error from every Analyze.
type fixedAnalyzer struct {
	diags []Diagnostic
	err   error
}

func (a fixedAnalyzer) Analyze(context.Context, *gogit.Worktree, []string, ...Diagnostic) ([]Diagnostic, error) {
	return a.diags, a.err
}

func TestRevalidate(t *testing.T) {
	errBump := errors.New("failed to fetch https://example.com/src.tar.xz: 404 Not Found")

	tests := []struct {
		name      string
		analyzer  fixedAnalyzer
		wantWants bool
		wantErr   error
	}{{
		name: "no diagnostics wants no change",
	}, {
		name:      "unfixed diagnostic wants a change",
		analyzer:  fixedAnalyzer{diags: []Diagnostic{{Path: "mod/go.mod", Rule: "outdated"}}},
		wantWants: true,
	}, {
		name:      "fixed diagnostic on a clean worktree wants no change",
		analyzer:  fixedAnalyzer{diags: []Diagnostic{{Path: "mod/go.mod", Rule: "informational", Fixed: true}}},
		wantWants: false,
	}, {
		name:      "change wanted but not applied wants a change",
		analyzer:  fixedAnalyzer{err: fmt.Errorf("updating package: %w: %w", ErrChangeWanted, errBump)},
		wantWants: true,
	}, {
		name:     "other analyzer error is returned",
		analyzer: fixedAnalyzer{err: fmt.Errorf("updating package: %w", errBump)},
		wantErr:  errBump,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initDefaultBranchRepo(t)
			cloneMgr, err := newTestCloneMeta(t).Get("owner", "repo")
			if err != nil {
				t.Fatalf("Get clone manager: %v", err)
			}
			r := &PRReconciler[*testRequest, *testResult, testCallbacks]{analyzer: tt.analyzer}

			wants, err := r.revalidate(t.Context(), cloneMgr, pathResource("mod/go.mod"), "main", 1)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("revalidate error: got = %v, want = %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("revalidate: %v", err)
			}
			if wants != tt.wantWants {
				t.Errorf("revalidate wants change: got = %t, want = %t", wants, tt.wantWants)
			}
		})
	}
}
