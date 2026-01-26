/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestClaudeToolHandler(t *testing.T) {
	meta, err := ClaudeTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("ClaudeTool returned error: %v", err)
	}

	if meta.Definition.Name != "submit_result" {
		t.Fatalf("unexpected tool name: %s", meta.Definition.Name)
	}

	ctx := context.Background()
	trace := evals.StartTrace[*sampleResult](ctx, "prompt")

	input := map[string]any{
		"reasoning": "done",
		"analysis": map[string]any{
			"summary": "all good",
		},
	}

	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	block := anthropic.ToolUseBlock{
		ID:    "tool-1",
		Name:  meta.Definition.Name,
		Input: payload,
	}

	var result *sampleResult
	resp := meta.Handler(ctx, block, trace, &result)
	if resp == nil {
		t.Fatalf("expected response")
	}
	if !resp["success"].(bool) {
		t.Fatalf("expected success response: %#v", resp)
	}
	if result == nil {
		t.Fatalf("expected result to be set")
	}
	if result.Summary != "all good" {
		t.Fatalf("unexpected result: %#v", result)
	}
}
