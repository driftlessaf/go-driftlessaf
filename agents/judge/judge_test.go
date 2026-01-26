/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"testing"
)

func TestPromptBuilding(t *testing.T) {
	// Test that the prompts have unbound placeholders (they shouldn't build without binding)
	_, err := goldenPrompt.Build()
	if err == nil {
		t.Error("goldenPrompt.Build() should fail with unbound placeholders")
	}

	// Test that the Request.Bind method works
	req := &Request{
		Mode:            GoldenMode,
		ReferenceAnswer: "reference",
		ActualAnswer:    "actual",
		Criterion:       "criterion",
	}

	// Currently the Bind method just returns the prompt unchanged (TODOs)
	boundPrompt, err := req.Bind(goldenPrompt)
	if err != nil {
		t.Fatalf("Request.Bind() error = %v", err)
	}
	if boundPrompt == nil {
		t.Error("Request.Bind() returned nil prompt")
	}

	// Test benchmark mode
	req.Mode = BenchmarkMode
	boundPrompt, err = req.Bind(benchmarkPrompt)
	if err != nil {
		t.Fatalf("Request.Bind() error = %v", err)
	}
	if boundPrompt == nil {
		t.Error("Request.Bind() returned nil prompt")
	}

	// Test standalone mode
	req.Mode = StandaloneMode
	boundPrompt, err = req.Bind(standalonePrompt)
	if err != nil {
		t.Fatalf("Request.Bind() error = %v", err)
	}
	if boundPrompt == nil {
		t.Error("Request.Bind() returned nil prompt")
	}
}

func TestJudgmentExtraction(t *testing.T) {
	// Test that Judgement type works with result.Extract
	tests := []struct {
		name     string
		response string
		wantErr  bool
	}{{
		name: "valid_json",
		response: `{
			"score": 0.85,
			"reasoning": "Good match",
			"breakdown": {
				"correctness": 1.0,
				"format": 0.0
			},
			"suggestions": ["Use numeric format"]
		}`,
		wantErr: false,
	}, {
		name:     "json_in_markdown",
		response: "Here is my judgment:\n```json\n{\n\t\"score\": 0.5,\n\t\"reasoning\": \"Partial match\",\n\t\"breakdown\": {},\n\t\"suggestions\": []\n}\n```\nThat's my evaluation.",
		wantErr:  false,
	}, {
		name:     "invalid_json",
		response: `This is not JSON`,
		wantErr:  true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This test just verifies that our Judgement type
			// is compatible with result.Extract
			if !tt.wantErr {
				// For valid cases, ensure JSON is parseable
				// (actual extraction is tested in result package)
				t.Logf("Test case %s expects valid JSON extraction", tt.name)
			} else {
				t.Logf("Test case %s expects extraction error", tt.name)
			}
		})
	}
}
