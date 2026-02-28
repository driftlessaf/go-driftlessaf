/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v75/github"
)

// testCallbacks is the standard tool composition: Empty -> Worktree -> Finding
type testCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

type testRequest struct {
	Findings []callbacks.Finding
}

func (r *testRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return p, nil
}

type testResult struct {
	commitMsg string
}

func (r *testResult) GetCommitMessage() string {
	return r.commitMsg
}

// fakeAgent implements metaagent.Agent for testing.
type fakeAgent struct {
	executeResult *testResult
	executeErr    error
}

func (a *fakeAgent) Execute(_ context.Context, _ *testRequest, _ testCallbacks) (*testResult, error) {
	return a.executeResult, a.executeErr
}

// fakeAnalyzer implements Analyzer for testing.
type fakeAnalyzer struct {
	diagnostics []Diagnostic
	err         error
}

func (a *fakeAnalyzer) Analyze(_ context.Context, _ *gogit.Worktree, _ ...string) ([]Diagnostic, error) {
	return a.diagnostics, a.err
}

func TestReconcilerFields(t *testing.T) {
	// Construct directly to avoid GCP metadata dependency in tests.
	rec := &Reconciler[*testRequest, *testResult, testCallbacks]{
		identity: "test-identity",
		analyzer: &fakeAnalyzer{},
		prLabels: []string{"label1", "label2"},
		agent:    &fakeAgent{},
		buildRequest: func(findings []callbacks.Finding) *testRequest {
			return &testRequest{Findings: findings}
		},
		buildCallbacks: func(_ *gogit.Worktree, _ *changemanager.Session[PRData]) testCallbacks {
			return testCallbacks{}
		},
	}

	if rec.identity != "test-identity" {
		t.Errorf("identity: got = %q, wanted = %q", rec.identity, "test-identity")
	}

	if len(rec.prLabels) != 2 {
		t.Errorf("len(prLabels): got = %d, wanted = 2", len(rec.prLabels))
	}

	if rec.prLabels[0] != "label1" {
		t.Errorf("prLabels[0]: got = %q, wanted = %q", rec.prLabels[0], "label1")
	}
}

func TestReconcilerWithEmptyLabels(t *testing.T) {
	rec := &Reconciler[*testRequest, *testResult, testCallbacks]{
		identity: "test-identity",
		analyzer: &fakeAnalyzer{},
		agent:    &fakeAgent{},
		buildRequest: func(_ []callbacks.Finding) *testRequest {
			return &testRequest{}
		},
		buildCallbacks: func(_ *gogit.Worktree, _ *changemanager.Session[PRData]) testCallbacks {
			return testCallbacks{}
		},
	}

	if len(rec.prLabels) != 0 {
		t.Errorf("prLabels: got = %v, wanted = empty", rec.prLabels)
	}
}

func TestPRDataFields(t *testing.T) {
	identity := fmt.Sprintf("test-%d", rand.Int64())
	path := fmt.Sprintf("path/to/go-%d.mod", rand.Int64())

	data := PRData{
		Identity: identity,
		Path:     path,
	}

	if data.Identity != identity {
		t.Errorf("PRData.Identity: got = %q, wanted = %q", data.Identity, identity)
	}

	if data.Path != path {
		t.Errorf("PRData.Path: got = %q, wanted = %q", data.Path, path)
	}
}

func TestResultInterface(t *testing.T) {
	msg := fmt.Sprintf("test-commit-%d", rand.Int64())

	var r Result = &testResult{commitMsg: msg}

	if got := r.GetCommitMessage(); got != msg {
		t.Errorf("Result.GetCommitMessage(): got = %q, wanted = %q", got, msg)
	}
}

func TestResultInterfaceWithEmptyMessage(t *testing.T) {
	var r Result = &testResult{commitMsg: ""}

	if got := r.GetCommitMessage(); got != "" {
		t.Errorf("Result.GetCommitMessage(): got = %q, wanted = empty string", got)
	}
}

func TestDiagnosticAsFinding(t *testing.T) {
	tests := []struct {
		name       string
		diagnostic Diagnostic
		wantID     string
		wantKind   callbacks.FindingKind
	}{{
		name: "with line number",
		diagnostic: Diagnostic{
			Path:    "pkg/foo.go",
			Line:    42,
			Message: "use slices.Contains",
			Rule:    "modernize",
		},
		wantID:   "modernize:pkg/foo.go:42",
		wantKind: callbacks.FindingKindCICheck,
	}, {
		name: "without line number",
		diagnostic: Diagnostic{
			Path:    "cmd/main.go",
			Line:    0,
			Message: "use range over int",
			Rule:    "modernize",
		},
		wantID:   "modernize:cmd/main.go",
		wantKind: callbacks.FindingKindCICheck,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finding := tt.diagnostic.AsFinding()

			if finding.Identifier != tt.wantID {
				t.Errorf("finding.Identifier: got = %q, wanted = %q", finding.Identifier, tt.wantID)
			}

			if finding.Kind != tt.wantKind {
				t.Errorf("finding.Kind: got = %q, wanted = %q", finding.Kind, tt.wantKind)
			}

			if finding.Details == "" {
				t.Error("finding.Details: got = empty, wanted non-empty")
			}
		})
	}
}

func TestCheckDetailsAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		details     CheckDetails
		wantCount   int
		wantPath    string
		wantLine    int
		wantTitle   string
		wantMessage string
	}{{
		name: "single diagnostic",
		details: CheckDetails{
			Diagnostics: []Diagnostic{{
				Path:    "pkg/foo.go",
				Line:    42,
				Message: "use slices.Contains",
				Rule:    "modernize",
			}},
		},
		wantCount:   1,
		wantPath:    "pkg/foo.go",
		wantLine:    42,
		wantTitle:   "modernize",
		wantMessage: "use slices.Contains",
	}, {
		name: "line zero defaults to 1",
		details: CheckDetails{
			Diagnostics: []Diagnostic{{
				Path:    "cmd/main.go",
				Line:    0,
				Message: "issue",
				Rule:    "rule",
			}},
		},
		wantCount: 1,
		wantLine:  1,
	}, {
		name:      "empty diagnostics",
		details:   CheckDetails{},
		wantCount: 0,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := tt.details.Annotations()
			if len(annotations) != tt.wantCount {
				t.Fatalf("len(annotations): got = %d, wanted = %d", len(annotations), tt.wantCount)
			}
			if tt.wantCount == 0 {
				return
			}
			a := annotations[0]
			if tt.wantPath != "" && a.GetPath() != tt.wantPath {
				t.Errorf("path: got = %q, wanted = %q", a.GetPath(), tt.wantPath)
			}
			if a.GetStartLine() != tt.wantLine {
				t.Errorf("start line: got = %d, wanted = %d", a.GetStartLine(), tt.wantLine)
			}
			if tt.wantTitle != "" && a.GetTitle() != tt.wantTitle {
				t.Errorf("title: got = %q, wanted = %q", a.GetTitle(), tt.wantTitle)
			}
			if tt.wantMessage != "" && a.GetMessage() != tt.wantMessage {
				t.Errorf("message: got = %q, wanted = %q", a.GetMessage(), tt.wantMessage)
			}
		})
	}
}

func TestCheckDetailsAnnotationsMaxLimit(t *testing.T) {
	diags := make([]Diagnostic, 60)
	for i := range diags {
		diags[i] = Diagnostic{
			Path:    fmt.Sprintf("file%d.go", i),
			Line:    i + 1,
			Message: "issue",
			Rule:    "rule",
		}
	}
	annotations := CheckDetails{Diagnostics: diags}.Annotations()
	if len(annotations) != maxAnnotations {
		t.Errorf("len(annotations): got = %d, wanted = %d", len(annotations), maxAnnotations)
	}
}

func TestCheckDetailsMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		details  CheckDetails
		contains []string
		empty    bool
	}{{
		name:    "empty diagnostics",
		details: CheckDetails{},
		empty:   true,
	}, {
		name: "with diagnostics and identity",
		details: CheckDetails{
			Diagnostics: []Diagnostic{{
				Path:    "pkg/foo.go",
				Line:    42,
				Message: "use slices.Contains",
				Rule:    "modernize",
			}},
			Identity: "my-bot",
		},
		contains: []string{
			"`pkg/foo.go`",
			"modernize",
			"use slices.Contains",
			"skip:my-bot",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := tt.details.Markdown()
			if tt.empty {
				if md != "" {
					t.Errorf("markdown: got = %q, wanted empty", md)
				}
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(md, want) {
					t.Errorf("markdown missing %q in:\n%s", want, md)
				}
			}
		})
	}
}

func TestParseDiff(t *testing.T) {
	rawDiff := `diff --git a/file.go b/file.go
index abc..def 100644
--- a/file.go
+++ b/file.go
@@ -10,3 +10,6 @@ func foo() {
+	newLine1
+	newLine2
+	newLine3
 	existingLine
 	existingLine2
 	existingLine3
`
	pd, err := parseDiff(rawDiff)
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	if len(pd.files) != 1 {
		t.Fatalf("len(files): got = %d, wanted = 1", len(pd.files))
	}
	if pd.files[0] != "file.go" {
		t.Errorf("files[0]: got = %q, wanted = %q", pd.files[0], "file.go")
	}
	// Three contiguous added lines should coalesce into a single range.
	if len(pd.ranges["file.go"]) != 1 {
		t.Fatalf("len(ranges[file.go]): got = %d, wanted = 1", len(pd.ranges["file.go"]))
	}
	if r := pd.ranges["file.go"][0]; r.start != 10 || r.end != 12 {
		t.Errorf("ranges[file.go][0]: got = {%d, %d}, wanted = {10, 12}", r.start, r.end)
	}
}

func TestParseDiffNonContiguous(t *testing.T) {
	// Added lines separated by an unchanged line should produce separate ranges.
	rawDiff := `diff --git a/file.go b/file.go
index abc..def 100644
--- a/file.go
+++ b/file.go
@@ -10,4 +10,6 @@ func foo() {
+	addedA
 	existing
+	addedB1
+	addedB2
 	existing2
`
	pd, err := parseDiff(rawDiff)
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	if len(pd.ranges["file.go"]) != 2 {
		t.Fatalf("len(ranges[file.go]): got = %d, wanted = 2", len(pd.ranges["file.go"]))
	}
	if r := pd.ranges["file.go"][0]; r.start != 10 || r.end != 10 {
		t.Errorf("ranges[file.go][0]: got = {%d, %d}, wanted = {10, 10}", r.start, r.end)
	}
	if r := pd.ranges["file.go"][1]; r.start != 12 || r.end != 13 {
		t.Errorf("ranges[file.go][1]: got = {%d, %d}, wanted = {12, 13}", r.start, r.end)
	}
}

func TestFilterToChangedLines(t *testing.T) {
	// Unified diff format with changes in file.go lines 10-15.
	rawDiff := `diff --git a/file.go b/file.go
index abc..def 100644
--- a/file.go
+++ b/file.go
@@ -10,3 +10,6 @@ func foo() {
+	newLine1
+	newLine2
+	newLine3
 	existingLine
 	existingLine2
 	existingLine3
`
	pd, err := parseDiff(rawDiff)
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}

	diagnostics := []Diagnostic{{
		Path: "file.go", Line: 12, Message: "in range", Rule: "r1",
	}, {
		Path: "file.go", Line: 50, Message: "out of range", Rule: "r2",
	}, {
		Path: "other.go", Line: 5, Message: "different file", Rule: "r3",
	}, {
		Path: "file.go", Line: 0, Message: "whole file", Rule: "r4",
	}}

	filtered := filterToChangedLines(diagnostics, pd)

	// Should include line 12 (in range) and line 0 (whole file).
	if len(filtered) != 2 {
		t.Fatalf("len(filtered): got = %d, wanted = 2", len(filtered))
	}
	if filtered[0].Line != 12 {
		t.Errorf("filtered[0].Line: got = %d, wanted = 12", filtered[0].Line)
	}
	if filtered[1].Line != 0 {
		t.Errorf("filtered[1].Line: got = %d, wanted = 0", filtered[1].Line)
	}
}

func TestFilterToChangedLinesExcludesContext(t *testing.T) {
	// Regression test: context lines within a diff hunk must not match.
	// This diff adds an import on line 19, with context lines around it.
	rawDiff := `diff --git a/file.go b/file.go
index abc..def 100644
--- a/file.go
+++ b/file.go
@@ -14,6 +14,7 @@ import (
 	"fmt"
 	"os"
 	"strings"
+	"testing"

 	"example.com/pkg"
 )
`
	pd, err := parseDiff(rawDiff)
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}

	diagnostics := []Diagnostic{{
		Path: "file.go", Line: 16, Message: "on context line", Rule: "r1",
	}, {
		Path: "file.go", Line: 17, Message: "on added line", Rule: "r2",
	}, {
		Path: "file.go", Line: 18, Message: "on context line after", Rule: "r3",
	}}

	filtered := filterToChangedLines(diagnostics, pd)

	// Only line 17 (the added line) should pass through.
	if len(filtered) != 1 {
		t.Fatalf("len(filtered): got = %d, wanted = 1", len(filtered))
	}
	if filtered[0].Line != 17 {
		t.Errorf("filtered[0].Line: got = %d, wanted = 17", filtered[0].Line)
	}
}

func TestFilterToChangedLinesEmptyDiff(t *testing.T) {
	pd, err := parseDiff("")
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}

	diagnostics := []Diagnostic{{Path: "file.go", Line: 1, Message: "msg", Rule: "r"}}

	// An empty diff has no changed lines, so all diagnostics are filtered out.
	filtered := filterToChangedLines(diagnostics, pd)
	if len(filtered) != 0 {
		t.Errorf("len(filtered): got = %d, wanted = 0", len(filtered))
	}
}

func TestHasLabel(t *testing.T) {
	tests := []struct {
		name      string
		labels    []string
		search    string
		wantFound bool
	}{{
		name:      "label found",
		labels:    []string{"bug", "skip:my-bot", "enhancement"},
		search:    "skip:my-bot",
		wantFound: true,
	}, {
		name:      "label not found",
		labels:    []string{"bug", "enhancement"},
		search:    "skip:my-bot",
		wantFound: false,
	}, {
		name:      "empty labels",
		labels:    nil,
		search:    "skip:my-bot",
		wantFound: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := newPRWithLabels(tt.labels)
			if got := hasLabel(pr, tt.search); got != tt.wantFound {
				t.Errorf("hasLabel: got = %v, wanted = %v", got, tt.wantFound)
			}
		})
	}
}

// newPRWithLabels creates a github.PullRequest with the given label names for testing.
func newPRWithLabels(names []string) *github.PullRequest {
	labels := make([]*github.Label, 0, len(names))
	for _, name := range names {
		labels = append(labels, &github.Label{Name: github.Ptr(name)})
	}
	return &github.PullRequest{Labels: labels}
}
