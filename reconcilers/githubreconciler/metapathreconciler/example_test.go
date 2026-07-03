/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler_test

import (
	"context"
	"fmt"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/issuemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/metapathreconciler"
	gogit "github.com/go-git/go-git/v5"
)

// Example_prData demonstrates the PRData type used for change detection.
// The PRData is embedded in PR bodies to track state across reconciliations.
func Example_prData() {
	data := metapathreconciler.PRData[any]{
		Identity: "my-bot",
	}

	fmt.Printf("Bot: %s\n", data.Identity)

	// Output:
	// Bot: my-bot
}

// Example_diagnostic demonstrates how Diagnostics are converted to Findings.
func Example_diagnostic() {
	diag := metapathreconciler.Diagnostic{
		Path:    "pkg/handler.go",
		Line:    42,
		Message: "use slices.Contains instead of manual loop",
		Rule:    "modernize",
	}

	finding := diag.AsFinding()
	fmt.Printf("Kind: %s\n", finding.Kind)
	fmt.Printf("ID: %s\n", finding.Identifier)

	// Output:
	// Kind: ciCheck
	// ID: modernize:pkg/handler.go:42
}

// reportOnlyAnalyzer illustrates an Analyzer that reports diagnostics
// without modifying the worktree, as issue mode requires.
type reportOnlyAnalyzer struct{}

func (reportOnlyAnalyzer) Analyze(context.Context, *gogit.Worktree, []string, ...metapathreconciler.Diagnostic) ([]metapathreconciler.Diagnostic, error) {
	return nil, nil
}

// Example_newIssues demonstrates wiring a reconciler that files findings as
// GitHub issues (for a downstream bot to remediate) instead of fixing them.
func Example_newIssues() {
	ctx := context.Background()
	const identity = "my-bot"

	// The issue manager carries the issue templates, the per-path cap, and
	// renders IssueData fields.
	titleTmpl := template.Must(template.New("title").Parse(`{{.Rule}} findings in {{.Path}}`))
	bodyTmpl := template.Must(template.New("body").Parse(`Fix the following:
{{range .Diagnostics}}- {{.Path}}:{{.Line}}: {{.Message}}
{{end}}`))
	im, err := issuemanager.New[metapathreconciler.IssueData](identity, titleTmpl, bodyTmpl,
		issuemanager.WithMaxDesiredIssuesPerPath[metapathreconciler.IssueData](10),
	)
	if err != nil {
		// handle error
		return
	}

	var cloneMeta *clonemanager.Meta // your clone manager metadata

	// The analyzer must be report-only; its diagnostics become issues, and
	// in ModeReview the same analyzer also reviews pull requests.
	rec, err := metapathreconciler.NewIssues(
		ctx,
		identity,
		reportOnlyAnalyzer{},
		im,
		cloneMeta,
		metapathreconciler.WithMode(metapathreconciler.ModeFix),
		// GroupByRule is the default; GroupByPath aggregates the whole path
		// into one issue.
		metapathreconciler.WithGrouping(metapathreconciler.GroupByRule),
		// The trigger label arms downstream remediation; use WithLabelFunc
		// instead to decide per repo/path.
		metapathreconciler.WithLabels("automated", "materializer/managed"),
		metapathreconciler.WithCloseMessage("These findings are no longer reported."),
	)
	if err != nil {
		// handle error
		return
	}
	_ = rec // register with githubreconciler.Main / CLIMain
}

// ExampleGroupByRule demonstrates the default grouping: one issue per rule,
// keyed by the rule name, with the diagnostics sorted deterministically.
func ExampleGroupByRule() {
	issues := metapathreconciler.GroupByRule(
		&githubreconciler.Resource{Path: "pkg/thing"},
		[]metapathreconciler.Diagnostic{
			{Path: "pkg/thing/b.go", Line: 9, Rule: "modernize", Message: "use min"},
			{Path: "pkg/thing/a.go", Line: 3, Rule: "gofmt", Message: "gofmt -s"},
			{Path: "pkg/thing/a.go", Line: 1, Rule: "modernize", Message: "use slices.Contains"},
		})
	for _, issue := range issues {
		fmt.Printf("%s: %d finding(s)\n", issue.Key, len(issue.Diagnostics))
	}

	// Output:
	// gofmt: 1 finding(s)
	// modernize: 2 finding(s)
}

// Example_diagnosticFixed demonstrates a Diagnostic that was fixed by the Analyzer.
func Example_diagnosticFixed() {
	diag := metapathreconciler.Diagnostic{
		Path:    "pkg/handler.go",
		Line:    42,
		Message: "use slices.Contains instead of manual loop",
		Rule:    "modernize",
		Fixed:   true,
	}

	fmt.Printf("Fixed: %v\n", diag.Fixed)

	// Output:
	// Fixed: true
}
