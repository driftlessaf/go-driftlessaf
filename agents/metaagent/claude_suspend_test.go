/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"encoding/json"
	"slices"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// TestSuspendToolContextIsImpactless pins the compatibility contract of the
// optional context property: only question is required, a question-only call
// extracts identically with or without the property declared, and the schema
// keys stay the ones checkpoint reads, so schema and extraction cannot drift.
func TestSuspendToolContextIsImpactless(t *testing.T) {
	tool := suspendToolParam("ask_friend", "ask a human for help")

	if got := tool.InputSchema.Required; !slices.Equal(got, []string{suspendQuestionProperty}) {
		t.Fatalf("Required = %v, want only %q: context must stay optional", got, suspendQuestionProperty)
	}
	props, ok := tool.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("Properties has type %T, want map[string]any", tool.InputSchema.Properties)
	}
	for _, key := range []string{suspendQuestionProperty, suspendContextProperty} {
		if _, ok := props[key]; !ok {
			t.Fatalf("schema is missing the %q property", key)
		}
	}

	// A question-only call (the pre-context shape) and a call carrying
	// context yield the same question through the checkpoint extraction, and
	// a supplied context round-trips through checkpoint.ContextFromPending,
	// which reads the same key the schema declares.
	const question = "what exact apk version should I pin?"
	const context = "tried python-3.12=3.12.9-r0; solver rejects it"
	for _, tc := range []struct {
		name        string
		input       map[string]string
		wantContext string
	}{
		{
			name:  "question-only",
			input: map[string]string{suspendQuestionProperty: question},
		},
		{
			name: "with-context",
			input: map[string]string{
				suspendQuestionProperty: question,
				suspendContextProperty:  context,
			},
			wantContext: context,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			calls := []checkpoint.PendingToolCall{{ID: "toolu_1", Name: "ask_friend", InputJSON: input}}
			if got := checkpoint.QuestionFromPending(calls); got != question {
				t.Fatalf("QuestionFromPending = %q, want %q", got, question)
			}
			if got := checkpoint.ContextFromPending(calls); got != tc.wantContext {
				t.Fatalf("ContextFromPending = %q, want %q", got, tc.wantContext)
			}
		})
	}
}
