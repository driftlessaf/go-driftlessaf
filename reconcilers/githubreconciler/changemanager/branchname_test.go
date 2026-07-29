/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
)

func TestBranchNameFor(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		res        *githubreconciler.Resource
		wantBranch string
		wantRef    string
		wantErr    bool
	}{{
		name:   "path resource with identity prefix",
		prefix: "test-bot",
		res: &githubreconciler.Resource{
			Type: githubreconciler.ResourceTypePath,
			Path: "linear-abc123",
			Ref:  "main",
		},
		wantBranch: "test-bot/linear-abc123",
		wantRef:    "main",
	}, {
		name:   "path resource with overridden prefix",
		prefix: "doc-driven",
		res: &githubreconciler.Resource{
			Type: githubreconciler.ResourceTypePath,
			Path: "linear-abc123",
			Ref:  "main",
		},
		wantBranch: "doc-driven/linear-abc123",
		wantRef:    "main",
	}, {
		name:   "issue resource with identity prefix",
		prefix: "test-bot",
		res: &githubreconciler.Resource{
			Type:   githubreconciler.ResourceTypeIssue,
			Number: 42,
			Ref:    "ignored",
		},
		wantBranch: "test-bot/issue-42",
		wantRef:    "main",
	}, {
		name:   "issue resource with overridden prefix",
		prefix: "doc-driven",
		res: &githubreconciler.Resource{
			Type:   githubreconciler.ResourceTypeIssue,
			Number: 42,
		},
		wantBranch: "doc-driven/issue-42",
		wantRef:    "main",
	}, {
		name:    "unsupported resource type",
		prefix:  "test-bot",
		res:     &githubreconciler.Resource{Type: githubreconciler.ResourceType("bogus")},
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, ref, err := branchNameFor(tt.prefix, tt.res)
			if (err != nil) != tt.wantErr {
				t.Fatalf("branchNameFor error: got = %v, wantErr = %v", err, tt.wantErr)
			}
			if branch != tt.wantBranch {
				t.Errorf("branch: got = %q, want = %q", branch, tt.wantBranch)
			}
			if ref != tt.wantRef {
				t.Errorf("ref: got = %q, want = %q", ref, tt.wantRef)
			}
		})
	}
}

func TestWithBranchPrefix(t *testing.T) {
	// The session config defaults to the CM identity; the option replaces it.
	sc := sessionConfig{branchPrefix: "test-bot"}
	WithBranchPrefix("doc-driven")(&sc)
	if got, want := sc.branchPrefix, "doc-driven"; got != want {
		t.Errorf("branchPrefix: got = %q, want = %q", got, want)
	}
}
