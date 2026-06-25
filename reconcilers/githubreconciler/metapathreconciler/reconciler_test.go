/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v88/github"
	"github.com/sethvargo/go-envconfig"
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

func TestEnvDecode(t *testing.T) {
	type config struct {
		Mode Mode `env:"TEST_MODE,required"`
	}

	tests := []struct {
		name    string
		val     string
		want    Mode
		wantErr bool
	}{{
		name: "fix",
		val:  "fix",
		want: ModeFix,
	}, {
		name: "review",
		val:  "review",
		want: ModeReview,
	}, {
		name: "all",
		val:  "all",
		want: ModeAll,
	}, {
		name: "none",
		val:  "none",
		want: ModeNone,
	}, {
		name: "case insensitive",
		val:  "FIX",
		want: ModeFix,
	}, {
		name: "whitespace trimmed",
		val:  "  review  ",
		want: ModeReview,
	}, {
		name: "config",
		val:  "config",
		want: ModeConfig,
	}, {
		name:    "unknown value",
		val:     "bogus",
		wantErr: true,
	}, {
		name:    "empty string",
		val:     "",
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg config
			err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
				Target:   &cfg,
				Lookuper: envconfig.MapLookuper(map[string]string{"TEST_MODE": tt.val}),
			})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != tt.want {
				t.Errorf("mode: got = %d, wanted = %d", cfg.Mode, tt.want)
			}
		})
	}
}

func TestModeString(t *testing.T) {
	tests := []struct {
		mode Mode
		want string
	}{{
		mode: ModeNone,
		want: "none",
	}, {
		mode: ModeFix,
		want: "fix",
	}, {
		mode: ModeReview,
		want: "review",
	}, {
		mode: ModeAll,
		want: "all",
	}, {
		mode: ModeConfig,
		want: "config",
	}, {
		mode: Mode(99),
		want: "unknown(99)",
	}}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.mode.String(); got != tt.want {
				t.Errorf("Mode.String(): got = %q, wanted = %q", got, tt.want)
			}
		})
	}
}

func TestWithMode(t *testing.T) {
	tests := []struct {
		name string
		mode Mode
		want Mode
	}{{
		name: "fix only",
		mode: ModeFix,
		want: ModeFix,
	}, {
		name: "review only",
		mode: ModeReview,
		want: ModeReview,
	}, {
		name: "all",
		mode: ModeAll,
		want: ModeAll,
	}, {
		name: "config",
		mode: ModeConfig,
		want: ModeConfig,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var o option
			WithMode(tt.mode)(&o)
			if o.mode != tt.want {
				t.Errorf("mode: got = %d, wanted = %d", o.mode, tt.want)
			}
		})
	}
}

func TestWithLabelFunc(t *testing.T) {
	var callCount int

	fn := func(_ context.Context, _ *githubreconciler.Resource, diags []Diagnostic, findings []callbacks.Finding) []string {
		callCount++
		var labels []string
		for _, d := range diags {
			labels = append(labels, "diag:"+d.Rule)
		}
		for _, f := range findings {
			labels = append(labels, "finding:"+string(f.Kind))
		}
		return labels
	}

	var o option
	WithLabelFunc(fn)(&o)

	if o.labelFn == nil {
		t.Fatal("labelFn: got = nil, wanted non-nil")
	}

	// First pass: diagnostics populated, no findings.
	diags := []Diagnostic{{Rule: "metadata-changed"}, {Rule: "version-update"}}
	got := o.labelFn(t.Context(), nil, diags, nil)
	if callCount != 1 {
		t.Fatalf("callCount: got = %d, wanted = 1", callCount)
	}
	want := []string{"diag:metadata-changed", "diag:version-update"}
	if len(got) != len(want) {
		t.Fatalf("labels len: got = %d, wanted = %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("labels[%d]: got = %q, wanted = %q", i, got[i], want[i])
		}
	}

	// Iteration pass: no diagnostics, findings populated.
	findings := []callbacks.Finding{
		{Kind: callbacks.FindingKindCICheck, Identifier: "check-1"},
		{Kind: callbacks.FindingKindReview, Identifier: "review-1"},
	}
	got = o.labelFn(t.Context(), nil, nil, findings)
	if callCount != 2 {
		t.Fatalf("callCount: got = %d, wanted = 2", callCount)
	}
	want = []string{"finding:ciCheck", "finding:review"}
	if len(got) != len(want) {
		t.Fatalf("labels len: got = %d, wanted = %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("labels[%d]: got = %q, wanted = %q", i, got[i], want[i])
		}
	}
}

func TestWithLabelFuncDefault(t *testing.T) {
	var o option
	if o.labelFn != nil {
		t.Error("default labelFn: got = non-nil, wanted = nil")
	}
}

func TestWithGiveUpComment(t *testing.T) {
	var o option
	WithGiveUpComment("<!--test:no-changes-->", func(e string) string { return "body: " + e })(&o)

	if o.giveUp == nil {
		t.Fatal("giveUp: got = nil, want non-nil")
	}
	if o.giveUp.Marker != "<!--test:no-changes-->" {
		t.Errorf("giveUp.Marker: got = %q, want = %q", o.giveUp.Marker, "<!--test:no-changes-->")
	}
	if o.giveUp.Render == nil {
		t.Fatal("giveUp.Render: got = nil, want non-nil")
	}
	if got := o.giveUp.Render("why"); got != "body: why" {
		t.Errorf("giveUp.Render: got = %q, want = %q", got, "body: why")
	}
}

func TestReconcileReviewOnlySkipsPath(t *testing.T) {
	rec := &Reconciler[*testRequest, *testResult, testCallbacks]{
		identity: "test-identity",
		mode:     ModeReview,
	}

	err := rec.Reconcile(t.Context(), &githubreconciler.Resource{
		Type:  githubreconciler.ResourceTypePath,
		Owner: "owner",
		Repo:  "repo",
	}, nil)
	if err != nil {
		t.Fatalf("Reconcile: got = %v, wanted = nil", err)
	}
}

func TestReconcileNoneModeSkipsPath(t *testing.T) {
	rec := &Reconciler[*testRequest, *testResult, testCallbacks]{
		identity: "test-identity",
		mode:     ModeNone,
	}

	err := rec.Reconcile(t.Context(), &githubreconciler.Resource{
		Type:  githubreconciler.ResourceTypePath,
		Owner: "owner",
		Repo:  "repo",
	}, nil)
	if err != nil {
		t.Fatalf("Reconcile: got = %v, wanted = nil", err)
	}
}

func TestIsConfig(t *testing.T) {
	tests := []struct {
		mode Mode
		want bool
	}{{
		mode: ModeConfig,
		want: true,
	}, {
		mode: ModeFix,
		want: false,
	}, {
		mode: ModeReview,
		want: false,
	}, {
		mode: ModeAll,
		want: false,
	}, {
		mode: ModeNone,
		want: false,
	}}

	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			if got := tt.mode.IsConfig(); got != tt.want {
				t.Errorf("IsConfig: got = %v, wanted = %v", got, tt.want)
			}
		})
	}
}

func newTestWorktree(t *testing.T) *gogit.Worktree {
	t.Helper()
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init test repo: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("get test worktree: %v", err)
	}
	return wt
}

func TestLoadRepoConfig(t *testing.T) {
	const identity = "test-bot"
	configPath := "." + identity + ".yaml"

	tests := []struct {
		name     string
		content  string // empty means no file
		wantMode Mode
		wantErr  bool
	}{{
		name:     "no file",
		content:  "",
		wantMode: ModeNone,
	}, {
		name:     "empty file",
		content:  "{}\n",
		wantMode: ModeFix,
	}, {
		name:     "mode fix",
		content:  "mode: fix\n",
		wantMode: ModeFix,
	}, {
		name:     "mode review",
		content:  "mode: review\n",
		wantMode: ModeReview,
	}, {
		name:     "mode all",
		content:  "mode: all\n",
		wantMode: ModeAll,
	}, {
		name:     "mode none",
		content:  "mode: none\n",
		wantMode: ModeNone,
	}, {
		name:    "invalid mode",
		content: "mode: bogus\n",
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wt := newTestWorktree(t)

			if tt.content != "" {
				f, err := wt.Filesystem.Create(configPath)
				if err != nil {
					t.Fatalf("create config: %v", err)
				}
				if _, err := fmt.Fprint(f, tt.content); err != nil {
					t.Fatalf("write config: %v", err)
				}
				f.Close()
			}

			m, err := loadRepoConfig(wt, identity)
			if tt.wantErr {
				if err == nil {
					t.Fatal("loadRepoConfig: got = nil, wanted error")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadRepoConfig: got = %v, wanted = nil", err)
			}
			if m != tt.wantMode {
				t.Errorf("mode: got = %q, wanted = %q", m, tt.wantMode)
			}
		})
	}
}

func TestReconcilerFields(t *testing.T) {
	// Construct directly to avoid GCP metadata dependency in tests.
	rec := &Reconciler[*testRequest, *testResult, testCallbacks]{
		identity: "test-identity",
		analyzer: &fakeAnalyzer{},
		prLabels: []string{"label1", "label2"},
		agent:    &fakeAgent{},
		buildRequest: func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *gogit.Worktree, findings []callbacks.Finding) (*testRequest, error) {
			return &testRequest{Findings: findings}, nil
		},
		buildCallbacks: func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
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
		buildRequest: func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *gogit.Worktree, _ []callbacks.Finding) (*testRequest, error) {
			return &testRequest{}, nil
		},
		buildCallbacks: func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
	}

	if len(rec.prLabels) != 0 {
		t.Errorf("prLabels: got = %v, wanted = empty", rec.prLabels)
	}
}

func TestPRDataFields(t *testing.T) {
	identity := fmt.Sprintf("test-%d", rand.Int64())
	path := fmt.Sprintf("path/to/go-%d.mod", rand.Int64())

	data := PRData[*testRequest]{
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
	}, {
		name: "fixed diagnostic",
		diagnostic: Diagnostic{
			Path:    "pkg/bar.go",
			Line:    10,
			Message: "use fmt.Appendf",
			Rule:    "modernize",
			Fixed:   true,
		},
		wantID:   "modernize:pkg/bar.go:10",
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

func TestCommitMessage(t *testing.T) {
	tests := []struct {
		name        string
		diagnostics []Diagnostic
		contains    []string
		notContains []string
	}{{
		name: "all fixed with line numbers",
		diagnostics: []Diagnostic{{
			Path: "pkg/foo.go", Line: 42, Message: "use slices.Contains", Rule: "modernize", Fixed: true,
		}, {
			Path: "cmd/main.go", Line: 10, Message: "use range over int", Rule: "modernize", Fixed: true,
		}},
		contains:    []string{"Automated fixes:", "modernize: pkg/foo.go:42 - use slices.Contains", "modernize: cmd/main.go:10 - use range over int"},
		notContains: nil,
	}, {
		name: "mixed fixed and unfixed",
		diagnostics: []Diagnostic{{
			Path: "pkg/foo.go", Line: 42, Message: "use slices.Contains", Rule: "modernize", Fixed: true,
		}, {
			Path: "cmd/main.go", Line: 10, Message: "complex refactor", Rule: "modernize", Fixed: false,
		}},
		contains:    []string{"modernize: pkg/foo.go:42"},
		notContains: []string{"complex refactor"},
	}, {
		name: "fixed without line number",
		diagnostics: []Diagnostic{{
			Path: "go.mod", Line: 0, Message: "outdated module", Rule: "deps", Fixed: true,
		}},
		contains: []string{"deps: go.mod - outdated module"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := commitMessage(tt.diagnostics)
			for _, want := range tt.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("commit message missing %q in:\n%s", want, msg)
				}
			}
			for _, notWant := range tt.notContains {
				if strings.Contains(msg, notWant) {
					t.Errorf("commit message should not contain %q in:\n%s", notWant, msg)
				}
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

func TestLoadFullRepoConfig(t *testing.T) {
	const identity = "test-bot"
	configPath := "." + identity + ".yaml"

	tests := []struct {
		name            string
		content         string // empty means no file
		wantMode        Mode
		wantExcludePats int
		wantErr         bool
	}{{
		name:     "no file",
		content:  "",
		wantMode: ModeNone,
	}, {
		name:     "mode only",
		content:  "mode: all\n",
		wantMode: ModeAll,
	}, {
		name: "with exclude patterns",
		content: "mode: all\n" +
			"exclude_patterns:\n" +
			"  - '.*/testdata/.*'\n",
		wantMode:        ModeAll,
		wantExcludePats: 1,
	}, {
		name:    "invalid exclude pattern",
		content: "exclude_patterns:\n  - '[invalid'\n",
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wt := newTestWorktree(t)

			if tt.content != "" {
				f, err := wt.Filesystem.Create(configPath)
				if err != nil {
					t.Fatalf("create config: %v", err)
				}
				if _, err := fmt.Fprint(f, tt.content); err != nil {
					t.Fatalf("write config: %v", err)
				}
				f.Close()
			}

			cfg, err := loadFullRepoConfig(wt, identity)
			if tt.wantErr {
				if err == nil {
					t.Fatal("loadFullRepoConfig: got = nil, wanted error")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadFullRepoConfig: got = %v, wanted = nil", err)
			}
			if cfg.Mode != tt.wantMode {
				t.Errorf("mode: got = %q, wanted = %q", cfg.Mode, tt.wantMode)
			}
			if len(cfg.excludePatterns) != tt.wantExcludePats {
				t.Errorf("len(excludePatterns): got = %d, wanted = %d", len(cfg.excludePatterns), tt.wantExcludePats)
			}
		})
	}
}

func TestApplyExcludeFilter(t *testing.T) {
	tests := []struct {
		name            string
		excludePatterns []string
		files           []string
		want            []string
	}{{
		name:  "no exclude patterns: all files pass through",
		files: []string{"pkg/foo.go", "bots/skillup/internal/agents/reviewer/testdata/files/myapp.yaml"},
		want:  []string{"pkg/foo.go", "bots/skillup/internal/agents/reviewer/testdata/files/myapp.yaml"},
	}, {
		name:            "testdata excluded: fixture filtered out",
		excludePatterns: []string{".*/testdata/.*"},
		files: []string{
			"pkg/foo.go",
			"bots/skillup/internal/agents/reviewer/testdata/files/myapp.yaml",
			"cmd/main.go",
		},
		want: []string{"pkg/foo.go", "cmd/main.go"},
	}, {
		name:            "all files excluded: empty result",
		excludePatterns: []string{".*/testdata/.*"},
		files:           []string{"a/testdata/b.yaml", "c/testdata/d.yaml"},
		want:            []string{},
	}, {
		name:            "no files: empty result",
		excludePatterns: []string{".*/testdata/.*"},
		files:           []string{},
		want:            []string{},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			excludePats, err := compileAnchoredPatterns(tt.excludePatterns)
			if err != nil {
				t.Fatalf("compileAnchoredPatterns: %v", err)
			}
			cfg := &fullRepoConfig{excludePatterns: excludePats}
			got := applyExcludeFilter(tt.files, cfg)
			if len(got) != len(tt.want) {
				t.Fatalf("applyExcludeFilter: got = %v, wanted = %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("applyExcludeFilter[%d]: got = %q, wanted = %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFullRepoConfigIsExcluded(t *testing.T) {
	tests := []struct {
		name            string
		excludePatterns []string
		path            string
		want            bool
	}{{
		name: "no exclude patterns: nothing excluded",
		path: "bots/skillup/internal/agents/reviewer/testdata/files/myapp.yaml",
		want: false,
	}, {
		name:            "testdata path is excluded",
		excludePatterns: []string{".*/testdata/.*"},
		path:            "bots/skillup/internal/agents/reviewer/testdata/files/myapp.yaml",
		want:            true,
	}, {
		name:            "non-testdata path is not excluded",
		excludePatterns: []string{".*/testdata/.*"},
		path:            "bots/skillup/pkg/reconciler/reconciler.go",
		want:            false,
	}, {
		name:            "go.mod not excluded by testdata pattern",
		excludePatterns: []string{".*/testdata/.*"},
		path:            "some/module/go.mod",
		want:            false,
	}, {
		name:            "non-go.mod file not excluded (path_patterns not applied)",
		excludePatterns: []string{".*/testdata/.*"},
		path:            "some/module/main.go",
		want:            false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			excludePats, err := compileAnchoredPatterns(tt.excludePatterns)
			if err != nil {
				t.Fatalf("compileAnchoredPatterns(excludePatterns): %v", err)
			}
			cfg := &fullRepoConfig{
				excludePatterns: excludePats,
			}
			if got := cfg.isExcluded(tt.path); got != tt.want {
				t.Errorf("isExcluded(%q): got = %v, wanted = %v", tt.path, got, tt.want)
			}
		})
	}
}

// mustCompilePatterns is a test helper that compiles anchored patterns or fails.
func mustCompilePatterns(t *testing.T, patterns []string) []*regexp.Regexp {
	t.Helper()
	compiled, err := compileAnchoredPatterns(patterns)
	if err != nil {
		t.Fatalf("compileAnchoredPatterns: %v", err)
	}
	return compiled
}

func TestSelectReviewFiles(t *testing.T) {
	testdataPat := ".*/testdata/.*"
	thirdPartyPat := ".*/third_party/.*"

	tests := []struct {
		name           string
		mode           Mode
		cfg            *fullRepoConfig
		files          []string
		wantKeep       []string
		wantTitle      string
		wantConclusion string // empty means term == nil (no short-circuit)
	}{{
		// P1: IsConfig mode → excludes applied, non-excluded files kept.
		name: "IsConfig mode: excludes applied",
		mode: ModeConfig,
		cfg: &fullRepoConfig{
			Mode:            ModeAll,
			excludePatterns: mustCompilePatterns(t, []string{testdataPat}),
		},
		files:    []string{"pkg/foo.go", "bots/skillup/testdata/fixture.yaml", "cmd/main.go"},
		wantKeep: []string{"pkg/foo.go", "cmd/main.go"},
	}, {
		// P1: IsConfig mode, config not review → neutral "Skipped (config)".
		name: "IsConfig mode: config not review → neutral",
		mode: ModeConfig,
		cfg: &fullRepoConfig{
			Mode: ModeFix,
		},
		files:          []string{"pkg/foo.go"},
		wantTitle:      "Skipped (config)",
		wantConclusion: "neutral",
	}, {
		// P1: IsConfig mode, nil cfg → neutral "Skipped (config)".
		name:           "IsConfig mode: nil cfg → neutral",
		mode:           ModeConfig,
		cfg:            nil,
		files:          []string{"pkg/foo.go"},
		wantTitle:      "Skipped (config)",
		wantConclusion: "neutral",
	}, {
		// P1: fixed ShouldReview mode (else if branch) → excludes applied.
		name: "ShouldReview mode: excludes applied",
		mode: ModeReview,
		cfg: &fullRepoConfig{
			Mode:            ModeReview,
			excludePatterns: mustCompilePatterns(t, []string{testdataPat}),
		},
		files:    []string{"pkg/foo.go", "bots/skillup/testdata/fixture.yaml"},
		wantKeep: []string{"pkg/foo.go"},
	}, {
		// P1: fixed mode + config-load error → fail-open (nil cfg, proceed without filtering).
		name:     "ShouldReview mode: nil cfg (load error) → fail-open",
		mode:     ModeReview,
		cfg:      nil,
		files:    []string{"pkg/foo.go", "bots/skillup/testdata/fixture.yaml"},
		wantKeep: []string{"pkg/foo.go", "bots/skillup/testdata/fixture.yaml"},
	}, {
		// P1+P2: all files excluded → short-circuit with neutral (not success).
		name: "all files excluded → neutral short-circuit",
		mode: ModeConfig,
		cfg: &fullRepoConfig{
			Mode:            ModeAll,
			excludePatterns: mustCompilePatterns(t, []string{testdataPat}),
		},
		files:          []string{"a/testdata/b.yaml", "c/testdata/d.yaml"},
		wantTitle:      "No files to analyze",
		wantConclusion: "neutral",
	}, {
		// P3: multiple patterns — file matching the second pattern is excluded.
		name: "multiple patterns: file matching second pattern excluded",
		mode: ModeReview,
		cfg: &fullRepoConfig{
			Mode:            ModeAll,
			excludePatterns: mustCompilePatterns(t, []string{testdataPat, thirdPartyPat}),
		},
		files:    []string{"pkg/foo.go", "vendor/third_party/lib/x.go", "cmd/main.go"},
		wantKeep: []string{"pkg/foo.go", "cmd/main.go"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keep, title, term := selectReviewFiles(tt.mode, tt.cfg, tt.files)

			if tt.wantConclusion != "" {
				// Expect a terminal short-circuit.
				if term == nil {
					t.Fatalf("selectReviewFiles: term = nil, wanted non-nil (conclusion %q)", tt.wantConclusion)
				}
				if term.Conclusion != tt.wantConclusion {
					t.Errorf("term.Conclusion: got = %q, wanted = %q", term.Conclusion, tt.wantConclusion)
				}
				if title != tt.wantTitle {
					t.Errorf("title: got = %q, wanted = %q", title, tt.wantTitle)
				}
				if term.Status != "completed" {
					t.Errorf("term.Status: got = %q, wanted = %q", term.Status, "completed")
				}
				return
			}

			// Expect no short-circuit.
			if term != nil {
				t.Fatalf("selectReviewFiles: term = %+v, wanted nil", term)
			}
			if title != "" {
				t.Errorf("title: got = %q, wanted empty", title)
			}
			if len(keep) != len(tt.wantKeep) {
				t.Fatalf("keep: got = %v, wanted = %v", keep, tt.wantKeep)
			}
			for i := range tt.wantKeep {
				if keep[i] != tt.wantKeep[i] {
					t.Errorf("keep[%d]: got = %q, wanted = %q", i, keep[i], tt.wantKeep[i])
				}
			}
		})
	}
}

// TestCompileAnchoredPatternsAnchoring verifies that patterns are anchored so
// a bare directory name does not match a deeper path (P3 hardening).
func TestCompileAnchoredPatternsAnchoring(t *testing.T) {
	// "foo" should match "foo" exactly but NOT "foo/bar".
	pats, err := compileAnchoredPatterns([]string{"foo"})
	if err != nil {
		t.Fatalf("compileAnchoredPatterns: %v", err)
	}
	if len(pats) != 1 {
		t.Fatalf("len(pats): got = %d, wanted = 1", len(pats))
	}
	re := pats[0]
	if !re.MatchString("foo") {
		t.Error("pattern 'foo' should match exact string 'foo'")
	}
	if re.MatchString("foo/bar") {
		t.Error("anchored pattern 'foo' must NOT match 'foo/bar' (dropped anchor would allow this)")
	}
}
