/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"encoding/json"
	"testing"
)

func TestRepoTargetUnmarshalDesignDoc(t *testing.T) {
	// Upstream state attachments may carry extra fields; we only extract
	// the repo-target fields. design_doc is optional and absent in older
	// upstream states.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"present", `{"repo":"owner/name","design_doc":"docs/some-team/thing.md"}`, "docs/some-team/thing.md"},
		{"absent", `{"repo":"owner/name"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var target RepoTarget
			if err := json.Unmarshal([]byte(tt.in), &target); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if target.DesignDoc != tt.want {
				t.Errorf("DesignDoc = %q, want %q", target.DesignDoc, tt.want)
			}
		})
	}
}

func TestPRDataDesignDocRoundTrip(t *testing.T) {
	// PRData is embedded as a JSON marker in PR bodies; DesignDoc must
	// survive the marshal/unmarshal round trip and old markers without
	// the field must decode to empty.
	in := PRData[struct{}]{Identity: "bot", LinearIssueID: "uuid", DesignDoc: "docs/some-team/thing.md"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out PRData[struct{}]
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DesignDoc != in.DesignDoc {
		t.Errorf("DesignDoc = %q, want %q", out.DesignDoc, in.DesignDoc)
	}

	// Old markers without the field must decode to empty.
	var old PRData[struct{}]
	if err := json.Unmarshal([]byte(`{"identity":"bot","linear_issue_id":"uuid"}`), &old); err != nil {
		t.Fatalf("unmarshal old marker: %v", err)
	}
	if old.DesignDoc != "" {
		t.Errorf("old marker: DesignDoc = %q, want empty", old.DesignDoc)
	}
}

func TestUpsertLabelsFor(t *testing.T) {
	base := []string{"bot/managed", "ai-review"}
	tests := []struct {
		name      string
		docLabel  string
		designDoc string
		wantLen   int
		wantLast  string
	}{
		{"label configured and doc present", "doc-driven", "docs/some-team/thing.md", 3, "doc-driven"},
		{"label configured, no doc", "doc-driven", "", 2, "ai-review"},
		{"label unconfigured, doc present", "", "docs/some-team/thing.md", 2, "ai-review"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upsertLabelsFor(base, tt.docLabel, tt.designDoc)
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d (%v)", len(got), tt.wantLen, got)
			}
			if got[len(got)-1] != tt.wantLast {
				t.Errorf("last = %q, want %q", got[len(got)-1], tt.wantLast)
			}
			// base must never be mutated (it's shared across reconciles).
			if len(base) != 2 {
				t.Errorf("base mutated: %v", base)
			}
		})
	}
}

func TestBranchPrefixSessionOpts(t *testing.T) {
	// A resolver in the shape bots use it: prefix targets that name a
	// design doc, keep identity branches for the rest.
	docResolver := func(target *RepoTarget) string {
		if target.DesignDoc != "" {
			return "doc-driven"
		}
		return ""
	}
	tests := []struct {
		name     string
		resolver BranchPrefixResolver
		target   *RepoTarget
		wantOpts int
	}{
		{"resolver matches target", docResolver, &RepoTarget{Repo: "org/repo", DesignDoc: "docs/some-team/thing.md"}, 1},
		{"resolver declines target", docResolver, &RepoTarget{Repo: "org/repo"}, 0},
		{"nil resolver", nil, &RepoTarget{Repo: "org/repo", DesignDoc: "docs/some-team/thing.md"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := branchPrefixSessionOpts(tt.resolver, tt.target)
			if len(got) != tt.wantOpts {
				t.Fatalf("len = %d, want %d", len(got), tt.wantOpts)
			}
		})
	}
}
