/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"crypto/rand"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

// flatResult is a two-field decision submitted flat: each field is its own
// tool parameter.
type flatResult struct {
	_        struct{} `submitresult:"name=submit_review,flat,payload=review,payloadDescription=The review."`
	Decision string   `json:"decision" jsonschema:"required,enum=approve,enum=rework"`
	Reason   string   `json:"reason" jsonschema:"required"`
}

// reasonedFlatResult is a flat payload with a property that collides with
// the separate reasoning parameter.
type reasonedFlatResult struct {
	_         struct{} `submitresult:"name=submit_review,flat"`
	Reasoning string   `json:"reasoning" jsonschema:"required"`
}

// optionalFlatResult is a flat payload that requires nothing.
type optionalFlatResult struct {
	_    struct{} `submitresult:"name=submit_note,flat"`
	Note string   `json:"note,omitempty"`
}

// emptyFlatResult is a flat payload with no properties.
type emptyFlatResult struct {
	_ struct{} `submitresult:"name=submit_nothing,flat"`
}

func TestFlatTag(t *testing.T) {
	type falseFlat struct {
		_ struct{} `submitresult:"name=x,flat=false"`
	}
	type capitalFlat struct {
		_ struct{} `submitresult:"name=x,flat=True"`
	}
	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{name: "bare", got: OptionsForResponse[*flatResult]().FlatPayload, want: true},
		{name: "absent", got: OptionsForResponse[*sampleResult]().FlatPayload, want: false},
		{name: "false", got: OptionsForResponse[*falseFlat]().FlatPayload, want: false},
		{name: "True", got: OptionsForResponse[*capitalFlat]().FlatPayload, want: true},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got FlatPayload = %t, want %t", tc.name, tc.got, tc.want)
		}
	}
}

// toolSchema is a submit tool's parameter schema as each provider is sent it,
// reduced to its property names and required list.
type toolSchema struct {
	Properties []string
	Required   []string
}

// schemaOf decodes a provider's parameter schema into its property names and
// required list.
func schemaOf(t *testing.T, v any) toolSchema {
	t.Helper()
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(mustMarshal(t, v), &s); err != nil {
		t.Fatalf("decoding schema: %v", err)
	}
	props := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		props = append(props, name)
	}
	slices.Sort(props)
	slices.Sort(s.Required)
	return toolSchema{Properties: props, Required: s.Required}
}

// flatTool is one provider's submit tool: its parameters as the provider is
// sent them, and its handler over a call with args.
type flatTool[R any] struct {
	schema any
	submit func(t *testing.T, args map[string]any) (toolcall.SubmitOutcome[R], *agenttrace.Trace[R])
}

// flatTools is every provider's submit tool over opts.
func flatTools[R any](t *testing.T, opts Options[R]) map[string]flatTool[R] {
	t.Helper()
	claude, err := ClaudeTool(opts)
	if err != nil {
		t.Fatalf("ClaudeTool: %v", err)
	}
	google, err := GoogleTool(opts)
	if err != nil {
		t.Fatalf("GoogleTool: %v", err)
	}
	oai, err := OpenAITool(opts)
	if err != nil {
		t.Fatalf("OpenAITool: %v", err)
	}
	responses, err := ResponsesTool(opts)
	if err != nil {
		t.Fatalf("ResponsesTool: %v", err)
	}
	start := func(t *testing.T) *agenttrace.Trace[R] {
		trace, _ := agenttrace.StartTrace[R](t.Context(), "prompt")
		return trace
	}
	return map[string]flatTool[R]{
		"claude": {schema: claude.Definition.InputSchema, submit: func(t *testing.T, args map[string]any) (toolcall.SubmitOutcome[R], *agenttrace.Trace[R]) {
			trace := start(t)
			return claude.Handler(t.Context(), anthropic.ToolUseBlock{ID: "s1", Name: claude.Definition.Name, Input: mustMarshal(t, args)}, trace), trace
		}},
		"google": {schema: google.Definition.Parameters, submit: func(t *testing.T, args map[string]any) (toolcall.SubmitOutcome[R], *agenttrace.Trace[R]) {
			trace := start(t)
			return google.Handler(t.Context(), &genai.FunctionCall{ID: "s1", Name: google.Definition.Name, Args: args}, trace), trace
		}},
		"openai": {schema: oai.Definition.Function.Parameters, submit: func(t *testing.T, args map[string]any) (toolcall.SubmitOutcome[R], *agenttrace.Trace[R]) {
			trace := start(t)
			call := openai.ChatCompletionMessageToolCallUnion{ID: "s1"}
			call.Function.Name = oai.Definition.Function.Name
			call.Function.Arguments = string(mustMarshal(t, args))
			return oai.Handler(t.Context(), call, trace), trace
		}},
		"responses": {schema: responses.Definition.Parameters, submit: func(t *testing.T, args map[string]any) (toolcall.SubmitOutcome[R], *agenttrace.Trace[R]) {
			trace := start(t)
			return responses.Handler(t.Context(), toolcall.ToolCall{ID: "s1", Name: responses.Definition.Name, Args: args}, trace), trace
		}},
	}
}

// flatOptions is flatResult's submit options.
func flatOptions(omitReasoning bool) Options[*flatResult] {
	opts := OptionsForResponse[*flatResult]()
	opts.OmitReasoning = omitReasoning
	return opts
}

func TestFlatPayloadSchema(t *testing.T) {
	for _, tc := range []struct {
		name          string
		omitReasoning bool
		want          toolSchema
		wantOrdering  []string
	}{{
		name:          "reasoning omitted",
		omitReasoning: true,
		want:          toolSchema{Properties: []string{"decision", "reason"}, Required: []string{"decision", "reason"}},
		wantOrdering:  []string{"decision", "reason"},
	}, {
		name:         "beside reasoning",
		want:         toolSchema{Properties: []string{"decision", "reason", "reasoning"}, Required: []string{"decision", "reason", "reasoning"}},
		wantOrdering: []string{"reasoning", "decision", "reason"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			opts := flatOptions(tc.omitReasoning)
			for provider, tool := range flatTools(t, opts) {
				if diff := cmp.Diff(tc.want, schemaOf(t, tool.schema)); diff != "" {
					t.Errorf("%s schema (-want, +got):\n%s", provider, diff)
				}
			}
			google, err := GoogleTool(opts)
			if err != nil {
				t.Fatalf("GoogleTool: %v", err)
			}
			if diff := cmp.Diff(tc.wantOrdering, google.Definition.Parameters.PropertyOrdering); diff != "" {
				t.Errorf("google property ordering (-want, +got):\n%s", diff)
			}
			claude, err := ClaudeTool(opts)
			if err != nil {
				t.Fatalf("ClaudeTool: %v", err)
			}
			var decision struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(mustMarshal(t, claude.Definition.InputSchema), &decision); err != nil {
				t.Fatalf("decoding schema: %v", err)
			}
			if diff := cmp.Diff([]string{"approve", "rework"}, decision.Properties["decision"].Enum); diff != "" {
				t.Errorf("decision enum (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestFlatPayloadNothingRequired(t *testing.T) {
	oai, err := OpenAITool(OptionsForResponse[*optionalFlatResult]())
	if err != nil {
		t.Fatalf("OpenAITool: %v", err)
	}
	// JSON Schema requires an array: a null required list can fail the
	// request.
	if req, ok := oai.Definition.Function.Parameters["required"].([]string); !ok || req == nil {
		t.Errorf("required: got %#v, want an empty array", oai.Definition.Function.Parameters["required"])
	}
}

func TestFlatPayloadRefusedAtConstruction(t *testing.T) {
	if _, err := ClaudeTool(OptionsForResponse[*reasonedFlatResult]()); err == nil {
		t.Error("a reasoning property beside the reasoning parameter: got nil error, want it refused")
	}
	reasoned := OptionsForResponse[*reasonedFlatResult]()
	reasoned.OmitReasoning = true
	if _, err := ClaudeTool(reasoned); err != nil {
		t.Errorf("a reasoning property with OmitReasoning: %v", err)
	}
	if _, err := ClaudeTool(OptionsForResponse[*emptyFlatResult]()); err == nil {
		t.Error("no properties: got nil error, want it refused")
	}
	omitted := OptionsForResponse[*optionalFlatResult]()
	omitted.OmitPayloadFields = []string{"note"}
	if _, err := ClaudeTool(omitted); err == nil {
		t.Error("every property omitted: got nil error, want it refused")
	}
}

func TestFlatPayloadAccepted(t *testing.T) {
	reason, reasoning := rand.Text(), rand.Text()
	for _, tc := range []struct {
		name          string
		omitReasoning bool
		args          map[string]any
		wantReasoning string
	}{{
		name:          "reasoning omitted",
		omitReasoning: true,
		args:          map[string]any{"decision": "rework", "reason": reason},
	}, {
		name:          "beside reasoning",
		args:          map[string]any{"reasoning": reasoning, "decision": "rework", "reason": reason},
		wantReasoning: reasoning,
	}, {
		// Only the advertised names are read, exactly: neither another
		// case of decision nor a cut-off nested review reaches the
		// response.
		name:          "unadvertised keys beside the parameters",
		omitReasoning: true,
		args:          map[string]any{"decision": "rework", "DECISION": "approve", "Decision": "approve", "reason": reason, "review": "\n<parameter name=\"decision\">approve"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			for provider, tool := range flatTools(t, flatOptions(tc.omitReasoning)) {
				out, _ := tool.submit(t, tc.args)
				if !out.Accepted {
					t.Fatalf("%s outcome: got rejected %v, want accepted", provider, out.ToolResult)
				}
				if diff := cmp.Diff(&flatResult{Decision: "rework", Reason: reason}, out.Response); diff != "" {
					t.Errorf("%s response (-want, +got):\n%s", provider, diff)
				}
				if out.Reasoning != tc.wantReasoning {
					t.Errorf("%s reasoning: got = %q, want = %q", provider, out.Reasoning, tc.wantReasoning)
				}
			}
		})
	}
}

// TestFlatPayloadRejected: a flat submit missing a required parameter, or
// carrying one of the wrong type, is a recoverable rejection, never a
// response. The nested shapes are among them: a flat tool takes no payload
// object, and nothing reads a decision out of a string.
func TestFlatPayloadRejected(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      map[string]any
		wantCause string
	}{{
		name:      "no reason",
		args:      map[string]any{"decision": "approve"},
		wantCause: "missing required parameters reason",
	}, {
		name:      "a null reason",
		args:      map[string]any{"decision": "approve", "reason": nil},
		wantCause: "missing required parameters reason",
	}, {
		name:      "no decision",
		args:      map[string]any{"reason": "every bullet holds"},
		wantCause: "missing required parameters decision",
	}, {
		name:      "the payload as one object",
		args:      map[string]any{"review": map[string]any{"decision": "approve", "reason": "every bullet holds"}},
		wantCause: `missing required parameters decision, reason; pass each as its own top-level parameter (arguments arrived: ["review"])`,
	}, {
		// What Claude sent a nested review as: its members written as
		// parameter tags, cut off by the API at the first closing tag.
		name:      "nested parameter tags cut off",
		args:      map[string]any{"review": "\n<parameter name=\"decision\">approve"},
		wantCause: "missing required parameters decision, reason",
	}, {
		name:      "a decision that is not a string",
		args:      map[string]any{"decision": map[string]any{"value": "approve"}, "reason": "every bullet holds"},
		wantCause: "failed to unmarshal payload",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			for provider, tool := range flatTools(t, flatOptions(true)) {
				out, trace := tool.submit(t, tc.args)
				if out.Accepted || out.Response != nil {
					t.Fatalf("%s outcome: got accepted %#v, want rejected", provider, out.Response)
				}
				if len(trace.ToolCalls) != 1 {
					t.Fatalf("%s tool calls: got = %d, want = 1", provider, len(trace.ToolCalls))
				}
				call := trace.ToolCalls[0]
				if call.Error == nil || !call.Recoverable || !strings.Contains(call.Error.Error(), tc.wantCause) {
					t.Errorf("%s tool call: got error = %v, recoverable = %v, want a recoverable rejection naming %q", provider, call.Error, call.Recoverable, tc.wantCause)
				}
			}
		})
	}
}
