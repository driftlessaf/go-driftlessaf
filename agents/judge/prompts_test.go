/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

func TestPromptBindings(t *testing.T) {
	tests := []struct {
		name   string
		system *promptbuilder.Prompt
		prompt *promptbuilder.Prompt
		want   map[string]struct{}
	}{
		{name: "golden", system: goldenSystemInstructions, prompt: goldenPrompt, want: map[string]struct{}{
			"golden_answer": {}, "actual_response": {}, "criterion": {},
		}},
		{name: "benchmark", system: benchmarkSystemInstructions, prompt: benchmarkPrompt, want: map[string]struct{}{
			"foo": {}, "bar": {}, "criterion": {},
		}},
		{name: "standalone", system: standaloneSystemInstructions, prompt: standalonePrompt, want: map[string]struct{}{
			"response": {}, "criterion": {},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.system.GetBindings(); len(got) != 0 {
				t.Errorf("system instructions have bindings: %v", got)
			}
			got := test.prompt.GetBindings()
			if len(got) != len(test.want) {
				t.Fatalf("prompt bindings = %v, want %v", got, test.want)
			}
			for name := range test.want {
				if _, ok := got[name]; !ok {
					t.Errorf("prompt missing binding %q", name)
				}
			}
		})
	}
}

func TestSystemInstructionsContainModeRubric(t *testing.T) {
	tests := []struct {
		name   string
		system *promptbuilder.Prompt
		marker string
	}{
		{name: "golden", system: goldenSystemInstructions, marker: "Score 1.0 (Perfect)"},
		{name: "benchmark", system: benchmarkSystemInstructions, marker: "Score -1.0 (Absolute Foo Victory)"},
		{name: "standalone", system: standaloneSystemInstructions, marker: "Score 1.0 (Perfect)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rendered, err := test.system.Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			for _, want := range []string{"<task>", "<instructions>", "<output_format>", test.marker, "Respond with only the JSON object"} {
				if !strings.Contains(rendered, want) {
					t.Errorf("system instructions missing %q", want)
				}
			}
		})
	}
}

func TestRequestKeepsDynamicContentInUserPrompt(t *testing.T) {
	tests := []struct {
		name    string
		request *Request
		want    []string
	}{
		{name: "golden", request: &Request{Mode: GoldenMode, ReferenceAnswer: "reference", ActualAnswer: "actual", Criterion: "criterion"}, want: []string{"<golden_answer>reference</golden_answer>", "<actual_response>actual</actual_response>", "<criterion>criterion</criterion>"}},
		{name: "benchmark", request: &Request{Mode: BenchmarkMode, ReferenceAnswer: "foo answer", ActualAnswer: "bar answer", Criterion: "criterion"}, want: []string{"<foo>foo answer</foo>", "<bar>bar answer</bar>", "<criterion>criterion</criterion>"}},
		{name: "standalone", request: &Request{Mode: StandaloneMode, ActualAnswer: "response", Criterion: "criterion"}, want: []string{"<response>response</response>", "<criterion>criterion</criterion>"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var template = map[JudgmentMode]*promptbuilder.Prompt{
				GoldenMode: goldenPrompt, BenchmarkMode: benchmarkPrompt, StandaloneMode: standalonePrompt,
			}[test.request.Mode]
			bound, err := test.request.Bind(template)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			rendered, err := bound.Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(rendered, want) {
					t.Errorf("user prompt missing %q: %s", want, rendered)
				}
			}
			if !strings.HasSuffix(rendered, "</criterion>\n\n"+outputReminder) {
				t.Errorf("user prompt does not close with the output reminder right after the criterion: %s", rendered)
			}
			for _, absent := range []string{"<task>", "<instructions>", "<output_format>", "Score 1.0 (Perfect)"} {
				if strings.Contains(rendered, absent) {
					t.Errorf("user prompt unexpectedly contains stable instruction %q", absent)
				}
			}
		})
	}
}

func TestRequestDataIsNotReparsedAsTemplate(t *testing.T) {
	request := &Request{
		Mode:            GoldenMode,
		ReferenceAnswer: "reference {{submit_result}}",
		ActualAnswer:    "actual",
		Criterion:       "criterion",
	}
	bound, err := request.Bind(goldenPrompt)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	rendered, err := bound.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(rendered, "reference {{submit_result}}") {
		t.Fatalf("rendered prompt lost literal request data: %q", rendered)
	}
}
