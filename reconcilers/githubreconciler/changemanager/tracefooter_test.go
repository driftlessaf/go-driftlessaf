/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/agents/agenttrace"
)

func newTestCM(t *testing.T, opts ...Option[struct{}]) *CM[struct{}] {
	t.Helper()
	tmpl := template.Must(template.New("t").Parse("x"))
	cm, err := New[struct{}]("test-bot", tmpl, tmpl, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cm
}

// TestTraceFooter_NoDashboardIsPlainText pins the default behavior: without
// WithTraceDashboard the footer is byte-for-byte the historical plain line,
// so existing bots see no body churn.
func TestTraceFooter_NoDashboardIsPlainText(t *testing.T) {
	cm := newTestCM(t)
	got := cm.traceFooter(t.Context(), "0a73c70eacefafe34d81015b8331d007")
	want := "\n\nTrace-ID: 0a73c70eacefafe34d81015b8331d007"
	if got != want {
		t.Errorf("traceFooter() = %q, want %q", got, want)
	}
}

// TestTraceFooter_LinksTraceID verifies the trace ID renders as a markdown
// link to the dashboard's trace view, and that without a reconciler key on
// the execution context no all-runs link is added.
func TestTraceFooter_LinksTraceID(t *testing.T) {
	cm := newTestCM(t, WithTraceDashboard[struct{}]("https://dash.example.com/agent-traces/"))
	got := cm.traceFooter(t.Context(), "0a73c70eacefafe34d81015b8331d007")
	want := "\n\nTrace-ID: [0a73c70eacefafe34d81015b8331d007](https://dash.example.com/agent-traces/?trace=0a73c70eacefafe34d81015b8331d007)"
	if got != want {
		t.Errorf("traceFooter() = %q, want %q", got, want)
	}
}

// TestTraceFooter_AddsAllRunsLink verifies that a reconciler key on the
// execution context adds the all-runs deep link, with the key query-escaped
// (path keys contain ':', '/', and '@').
func TestTraceFooter_AddsAllRunsLink(t *testing.T) {
	cm := newTestCM(t, WithTraceDashboard[struct{}]("https://dash.example.com/agent-traces/"))
	ctx := agenttrace.WithExecutionContext(t.Context(), agenttrace.ExecutionContext{
		ReconcilerKey: "path:chainguard-dev/mono@main:bots/cve-remediation",
	})
	got := cm.traceFooter(ctx, "0a73c70eacefafe34d81015b8331d007")

	if !strings.Contains(got, "[all agent runs for this PR](https://dash.example.com/agent-traces/?reconcile=path%3Achainguard-dev%2Fmono%40main%3Abots%2Fcve-remediation)") {
		t.Errorf("traceFooter() missing escaped all-runs link:\n%s", got)
	}
	if !strings.Contains(got, "?trace=0a73c70eacefafe34d81015b8331d007") {
		t.Errorf("traceFooter() missing trace link:\n%s", got)
	}
}

// TestTraceFooter_PreservesBaseQuery verifies parameters pinned on the base
// URL (e.g. an environment selector) survive on both links.
func TestTraceFooter_PreservesBaseQuery(t *testing.T) {
	cm := newTestCM(t, WithTraceDashboard[struct{}]("https://dash.example.com/agent-traces/?env=staging"))
	ctx := agenttrace.WithExecutionContext(t.Context(), agenttrace.ExecutionContext{
		ReconcilerKey: "pr:chainguard-dev/mono/45101",
	})
	got := cm.traceFooter(ctx, "deadbeef")

	for _, want := range []string{
		"env=staging&trace=deadbeef",
		"env=staging&reconcile=pr%3Achainguard-dev%2Fmono%2F45101",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("traceFooter() missing %q:\n%s", want, got)
		}
	}
}

// TestTraceFooter_InvalidBaseFallsBackToPlain verifies a malformed dashboard
// URL degrades to the plain footer instead of emitting a broken link.
func TestTraceFooter_InvalidBaseFallsBackToPlain(t *testing.T) {
	cm := newTestCM(t, WithTraceDashboard[struct{}]("://not-a-url"))
	got := cm.traceFooter(t.Context(), "deadbeef")
	want := "\n\nTrace-ID: deadbeef"
	if got != want {
		t.Errorf("traceFooter() = %q, want %q", got, want)
	}
}
