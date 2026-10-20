/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"os"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/anthropics/anthropic-sdk-go"
)

// genomeResult mirrors the shape of mentat's submit_genome payload, whose
// field is named result, which is what the recorded fixtures carry.
type genomeResult struct {
	_ struct{} `submitresult:"name=submit_genome,payload=result,description=Submit the genome.,payloadDescription=Genome result"`

	LogEntry      string `json:"log_entry"`
	WaiverReasons []struct {
		ID string `json:"id"`
	} `json:"waiver_reasons"`
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(b)
}

// TestClaudeSubmitRecoversPayloadEmbeddedAsJSONInReasoning pins the shape of
// the gevent, hystrix and jline submits that failed with "result parameter
// is required": the model wrote the whole argument object as the reasoning
// text, so the result object arrived as a JSON member inside reasoning. The
// fixtures are the reasoning strings of trace 20261020-163121-05a82e3e.
func TestClaudeSubmitRecoversPayloadEmbeddedAsJSONInReasoning(t *testing.T) {
	submit, err := ClaudeToolForResponse[*genomeResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		// wantLogEntry is set when the submit must be accepted.
		wantLogEntry      string
		wantWaiverID      string
		wantReasoningTail string
		wantHint          []string
		wantNoHint        string
	}{{
		name:              "recovered from the recorded gevent submit",
		args:              map[string]any{"reasoning": readFixture(t, "gevent-reasoning-embedded-result.txt")},
		wantLogEntry:      "Fix BUILD_STEP_FAILED",
		wantWaiverID:      "wb2a5a56c6aaecf066d0da94f4b72d5b8",
		wantReasoningTail: "for which I supply the required waiver_reasons entry.",
	}, {
		name: "result field closed outside the object keeps a hint",
		args: map[string]any{"reasoning": readFixture(t, "gevent-reasoning-field-outside-result.txt")},
		wantHint: []string{
			"result parameter is required",
			"the result was found inside reasoning at offset 1641",
			"content other than closing brackets follows its closing brace",
			"pass the result as the top-level result parameter",
		},
	}, {
		name: "truncated embedded object keeps a hint",
		args: map[string]any{"reasoning": "diagnosed it\",\n\"result\": {\n\"log_entry\": \"Fix the bu"},
		wantHint: []string{
			"the result was found inside reasoning at offset 15",
			"it is not a complete JSON object",
			"pass the result as the top-level result parameter",
		},
	}, {
		name:       "truly missing result is unchanged",
		args:       map[string]any{"reasoning": "diagnosed it and fixed the build"},
		wantHint:   []string{"result parameter is required"},
		wantNoHint: "inside reasoning",
	}, {
		name:       "quoted field name without an object is not a payload",
		args:       map[string]any{"reasoning": `the "result": field stays empty`},
		wantHint:   []string{"result parameter is required"},
		wantNoHint: "inside reasoning",
	}, {
		name: "present result is never overridden",
		args: map[string]any{
			"reasoning": "diagnosed it\",\n\"result\": {\"log_entry\": \"from reasoning\"}\n}",
			"result":    map[string]any{"log_entry": "from the parameter"},
		},
		wantLogEntry:      "from the parameter",
		wantReasoningTail: "diagnosed it\",\n\"result\": {\"log_entry\": \"from reasoning\"}\n}",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			trace, _ := agenttrace.StartTrace[*genomeResult](ctx, "prompt")
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, tc.args)}

			outcome := submit.Handler(ctx, block, trace)

			if tc.wantLogEntry == "" {
				if outcome.Accepted {
					t.Fatalf("got = accepted (%#v), want = rejected", outcome.Response)
				}
				hint, _ := outcome.ToolResult["error"].(string)
				for _, want := range tc.wantHint {
					if !strings.Contains(hint, want) {
						t.Errorf("tool result error: got = %q, want = one containing %q", hint, want)
					}
				}
				if tc.wantNoHint != "" && strings.Contains(hint, tc.wantNoHint) {
					t.Errorf("tool result error: got = %q, want = one without %q", hint, tc.wantNoHint)
				}
				if len(trace.ToolCalls) != 1 || trace.ToolCalls[0].Error == nil {
					t.Fatalf("tool calls: got = %#v, want = one rejected call", trace.ToolCalls)
				}
				return
			}

			if !outcome.Accepted {
				t.Fatalf("got = rejected (%#v), want = accepted", outcome.ToolResult)
			}
			if got := outcome.Response.LogEntry; !strings.HasPrefix(got, tc.wantLogEntry) {
				t.Errorf("log entry: got = %q, want = one starting %q", got, tc.wantLogEntry)
			}
			if tc.wantWaiverID != "" {
				if got := outcome.Response.WaiverReasons; len(got) != 1 || got[0].ID != tc.wantWaiverID {
					t.Errorf("waiver reasons: got = %#v, want = one with id %q", got, tc.wantWaiverID)
				}
			}
			if got := outcome.Reasoning; !strings.HasSuffix(got, tc.wantReasoningTail) {
				t.Errorf("reasoning: got = one ending %q, want = one ending %q", got[max(len(got)-80, 0):], tc.wantReasoningTail)
			}
		})
	}
}
