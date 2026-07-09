/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestWithExecutionContext_preservesUpstreamFields(t *testing.T) {
	// Guards against the original manifest-gen footgun: a deep call site that
	// owned one field (TurnNumber) and called WithExecutionContext with a
	// partial struct used to wipe ReconcilerKey/ReconcilerType/CommitSHA set
	// by the enclosing reconciler. Non-zero-merge semantics must preserve
	// untouched fields.
	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		ReconcilerKey:  "pr:chainguard-dev/mono/40044",
		ReconcilerType: "pr",
		CommitSHA:      "0adc2e9",
	})

	ctx = WithExecutionContext(ctx, ExecutionContext{TurnNumber: 3})

	got := GetExecutionContext(ctx)
	if got.ReconcilerKey != "pr:chainguard-dev/mono/40044" {
		t.Errorf("ReconcilerKey clobbered: %q", got.ReconcilerKey)
	}
	if got.ReconcilerType != "pr" {
		t.Errorf("ReconcilerType clobbered: %q", got.ReconcilerType)
	}
	if got.CommitSHA != "0adc2e9" {
		t.Errorf("CommitSHA clobbered: %q", got.CommitSHA)
	}
	if got.TurnNumber != 3 {
		t.Errorf("TurnNumber not applied: %d", got.TurnNumber)
	}
}

func TestWithExecutionContext_overridesOnNonZero(t *testing.T) {
	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		ReconcilerKey: "pr:a/b/1",
		CommitSHA:     "old-sha",
		TurnNumber:    1,
	})

	ctx = WithExecutionContext(ctx, ExecutionContext{
		CommitSHA:  "new-sha",
		TurnNumber: 2,
	})

	got := GetExecutionContext(ctx)
	if got.ReconcilerKey != "pr:a/b/1" {
		t.Errorf("ReconcilerKey should remain: %q", got.ReconcilerKey)
	}
	if got.CommitSHA != "new-sha" {
		t.Errorf("CommitSHA should be overridden: %q", got.CommitSHA)
	}
	if got.TurnNumber != 2 {
		t.Errorf("TurnNumber should be overridden: %d", got.TurnNumber)
	}
}

func TestWithExecutionContext_mergesLabels(t *testing.T) {
	// A deep call site adding one label must not drop labels set by the
	// enclosing reconciler, and overlapping keys take the newest value.
	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		Labels: map[string]string{"genai_component": "analyzer", "purl_type": "npm"},
	})
	ctx = WithExecutionContext(ctx, ExecutionContext{
		Labels: map[string]string{"purl_type": "pypi", "turn_tag": "x"},
	})

	got := GetExecutionContext(ctx)
	want := map[string]string{"genai_component": "analyzer", "purl_type": "pypi", "turn_tag": "x"}
	if len(got.Labels) != len(want) {
		t.Fatalf("Labels = %v, want %v", got.Labels, want)
	}
	for k, v := range want {
		if got.Labels[k] != v {
			t.Errorf("Labels[%q] = %q, want %q", k, got.Labels[k], v)
		}
	}
}

func TestWithExecutionContext_doesNotMutateCallerLabels(t *testing.T) {
	// The caller's map must never be mutated by a downstream merge.
	base := map[string]string{"genai_component": "analyzer"}
	ctx := WithExecutionContext(t.Context(), ExecutionContext{Labels: base})
	_ = WithExecutionContext(ctx, ExecutionContext{Labels: map[string]string{"purl_type": "npm"}})

	if _, mutated := base["purl_type"]; mutated {
		t.Errorf("caller's Labels map was mutated: %v", base)
	}
}

func TestEnrichAttributes_customLabels(t *testing.T) {
	e := ExecutionContext{
		ReconcilerType: "pr",
		Labels:         map[string]string{"genai_component": "analyzer", "purl_type": "npm"},
	}
	got := map[string]string{}
	for _, kv := range e.EnrichAttributes([]attribute.KeyValue{attribute.String("model", "claude")}) {
		got[string(kv.Key)] = kv.Value.AsString()
	}

	for k, want := range map[string]string{
		"model":           "claude",
		"reconciler_type": "pr",
		"genai_component": "analyzer",
		"purl_type":       "npm",
	} {
		if got[k] != want {
			t.Errorf("attribute %q = %q, want %q", k, got[k], want)
		}
	}
}

func TestEnrichAttributes_excludesRequestID(t *testing.T) {
	// RequestID is high-cardinality and must never reach metrics: EnrichAttributes
	// must not emit it, even though it rides on the trace's exec_context.
	e := ExecutionContext{
		RequestID: "req-9f3a",
		Labels:    map[string]string{"genai_component": "analyzer"},
	}
	for _, kv := range e.EnrichAttributes(nil) {
		if string(kv.Key) == "request_id" || kv.Value.AsString() == "req-9f3a" {
			t.Errorf("EnrichAttributes leaked request_id onto metrics: %v", kv)
		}
	}
}

func TestWithExecutionContext_mergesRequestID(t *testing.T) {
	// A deep call site setting RequestID must not clobber the enclosing
	// reconciler's other fields, and a later non-empty RequestID overrides.
	ctx := WithExecutionContext(t.Context(), ExecutionContext{ReconcilerType: "pr"})
	ctx = WithExecutionContext(ctx, ExecutionContext{RequestID: "req-1"})
	if got := GetExecutionContext(ctx); got.RequestID != "req-1" || got.ReconcilerType != "pr" {
		t.Fatalf("got %+v, want RequestID=req-1 ReconcilerType=pr", got)
	}

	ctx = WithExecutionContext(ctx, ExecutionContext{RequestID: "req-2"})
	if got := GetExecutionContext(ctx); got.RequestID != "req-2" {
		t.Errorf("RequestID = %q, want req-2", got.RequestID)
	}
}

func TestWithExecutionContext_emptyCtx(t *testing.T) {
	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		ReconcilerKey:  "path:a/b@main:c",
		ReconcilerType: "path",
	})

	got := GetExecutionContext(ctx)
	if got.ReconcilerKey != "path:a/b@main:c" || got.ReconcilerType != "path" {
		t.Errorf("merge on empty ctx failed: %+v", got)
	}
}
