/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	gogit "github.com/go-git/go-git/v5"
)

// Result is implemented by all agent result types.
// The commit message is used when pushing changes to the repository.
type Result interface {
	GetCommitMessage() string
}

// Analyzer runs a static analysis tool over a worktree and returns diagnostics.
// Each path is relative to the repo root (e.g., "path/to/package").
type Analyzer interface {
	// Analyze runs the tool scoped to the given paths within the worktree
	// and returns diagnostics. An empty slice means the paths are clean.
	//
	// prior carries findings from a previous pass so analyzers with
	// nondeterministic output (e.g. agent-based audits) can re-confirm
	// tracked findings under stable identities rather than re-discovering
	// them under new ones. Deterministic analyzers ignore it, and callers
	// with no prior state pass none.
	Analyze(ctx context.Context, wt *gogit.Worktree, paths []string, prior ...Diagnostic) ([]Diagnostic, error)
}

// Diagnostic represents a single issue discovered by an Analyzer.
type Diagnostic struct {
	// Path is the file path relative to the repo root.
	Path string `json:"path" jsonschema:"description=File path relative to the repo root"`

	// Line is the line number (0 if not applicable).
	Line int `json:"line" jsonschema:"description=Line number of the issue (0 if not applicable)"`

	// Message is a human-readable description of the issue.
	Message string `json:"message" jsonschema:"description=Human-readable description of the issue"`

	// Rule is the specific check/rule ID (e.g., "S1000", "modernize").
	Rule string `json:"rule" jsonschema:"description=The rule that was violated"`

	// Fixed indicates that the Analyzer already applied a fix for this
	// diagnostic by modifying files in the worktree. Fixed diagnostics
	// are not passed to the agent as findings.
	Fixed bool `json:"fixed,omitempty" jsonschema:"description=Whether the analyzer already fixed this issue"`
}

// AsFinding converts a Diagnostic into a Finding so that diagnostics and
// CI/review findings can be combined into a single slice for the metaagent.
func (d Diagnostic) AsFinding() callbacks.Finding {
	id := d.Rule + ":" + d.Path
	if d.Line > 0 {
		id += fmt.Sprintf(":%d", d.Line)
	}
	details := d.Path
	if d.Line > 0 {
		details += fmt.Sprintf(":%d", d.Line)
	}
	details += ": " + d.Message

	return callbacks.Finding{
		Kind:       callbacks.FindingKindCICheck,
		Identifier: id,
		Name:       id,
		Details:    details,
	}
}

// commitMessage builds a commit message enumerating the fixed diagnostics.
func commitMessage(diagnostics []Diagnostic) string {
	var sb strings.Builder
	sb.WriteString("Automated fixes:\n")
	for _, d := range diagnostics {
		if !d.Fixed {
			continue
		}
		sb.WriteString("\n- ")
		sb.WriteString(d.Rule)
		sb.WriteString(": ")
		sb.WriteString(d.Path)
		if d.Line > 0 {
			fmt.Fprintf(&sb, ":%d", d.Line)
		}
		sb.WriteString(" - ")
		sb.WriteString(d.Message)
	}
	return sb.String()
}

// PRData is the data embedded in PR bodies for change detection.
// This is used by the changemanager to track state across reconciliations.
// It is parameterized by the request type so that request data can be
// incorporated into PR title and body templates. The Request field
// serializes as "request" and participates in state comparisons, so
// fields that vary reconcile to reconcile (e.g. findings) should use
// json:"-" in the concrete request type.
type PRData[Req any] struct {
	Identity string `json:"identity"`
	Path     string `json:"path"`
	Request  Req    `json:"request"`

	// ReasoningSummary is the rendered per-commit reasoning log for this PR:
	// one markdown block per bot commit — the commit headline in bold over
	// that iteration's truncated reasoning summary — accumulated across
	// iterations via the reasoning log the session persists in the PR body
	// (see changemanager.Session.AppendReasoning). Populated by the
	// reconciler when a commit is created and empty when no iteration
	// recorded reasoning. Excluded from JSON so it never participates in
	// change detection (it varies run to run). Render it by appending
	// [ReasoningSummarySnippet] to the PR body template.
	ReasoningSummary string `json:"-"`

	// Headline is the commit headline anchoring the PR title: the first
	// reasoning-log entry's headline (the PR's primary change, stable across
	// follow-up iterations), falling back to the current run's commit
	// headline when the log is empty. Populated by the reconciler when an
	// agent commit is created; empty for analyzer-only commits, so title
	// templates using it need an {{else}} branch. Unlike Path — the
	// reconciled resource that triggered the run — it describes what the
	// agent actually changed, which may be entirely different files.
	// Excluded from JSON so it never participates in change detection.
	Headline string `json:"-"`
}
