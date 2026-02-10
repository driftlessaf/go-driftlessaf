/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"google.golang.org/genai"
)

func TestGoogleToolHandler(t *testing.T) {
	meta, err := GoogleTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("GoogleTool returned error: %v", err)
	}

	if meta.Definition.Name != "submit_result" {
		t.Fatalf("unexpected tool name: %s", meta.Definition.Name)
	}

	ctx := context.Background()
	trace := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := &genai.FunctionCall{
		ID:   "call-1",
		Name: meta.Definition.Name,
		Args: map[string]any{
			"reasoning": "done",
			"analysis": map[string]any{
				"summary": "all good",
			},
		},
	}

	var result *sampleResult
	resp := meta.Handler(ctx, call, trace, &result)
	if resp == nil {
		t.Fatalf("expected response")
	}
	if success, ok := resp.Response["success"].(bool); !ok || !success {
		t.Fatalf("expected success response: %#v", resp.Response)
	}
	if result == nil {
		t.Fatalf("expected result to be set")
	}
	if result.Summary != "all good" {
		t.Fatalf("unexpected result: %#v", result)
	}
}
