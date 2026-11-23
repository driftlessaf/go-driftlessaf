/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"
)

// Stand-ins for confidential caller input: a tool argument holding a string
// from the caller's own input, and a model-written justification repeating it.
// A neutral token would not exercise the paraphrase case the policy exists for.
const (
	sensitivePattern   = "restricted-input-7742"
	sensitiveReasoning = "Confirm 4.1.0 still references restricted-input-7742 in lib/index.ts."
	publicRef          = "4.1.0"
)

// functionCallTurnJSON renders an assistant turn calling toolName with args.
// The executor emits its "Executing tool call" line before it dispatches, so
// this drives the line whether or not a handler is registered for the name.
func functionCallTurnJSON(t *testing.T, callID, toolName string, args map[string]any) string {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return fmt.Sprintf(`{
		"candidates":[{"content":{"parts":[
			{"functionCall":{"id":%q,"name":%q,"args":%s}}
		]}}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
	}`, callID, toolName, argsJSON)
}

// gitGrepTool builds a git_grep tool through the real googletool.FromTool, so a
// policy that registration declares but the conversion drops fails these tests.
func gitGrepTool(policy *toolcall.ArgLogPolicy) googletool.Metadata[errCapResponse] {
	return googletool.FromTool(toolcall.Tool[errCapResponse]{
		Def: toolcall.Definition{
			Name:        "git_grep",
			Description: "Search the tree at a ref for a pattern.",
			Parameters: []toolcall.Parameter{
				{Name: "ref", Type: "string", Description: "The ref to search.", Required: true},
				{Name: "pattern", Type: "string", Description: "The text to search for.", Required: true},
			},
			LogPolicy: policy,
		},
		Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
			return map[string]any{"matches": ""}
		},
	})
}

// runToolCall drives a real executor configured with opts through one call to
// toolName with args, then a submit, and returns everything written to the
// context's log sink. A toolName of submit_result ends the run on that call.
func runToolCall(t *testing.T, tools map[string]googletool.Metadata[errCapResponse], toolName string, args map[string]any, opts ...googleexecutor.Option[errCapRequest, errCapResponse]) *bytes.Buffer {
	t.Helper()
	logs, err := runToolCallErr(t, tools, toolName, args, opts...)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return logs
}

// runToolCallErr is runToolCall for a run expected to end in an error, such
// as the *checkpoint.Suspension a suspend call returns.
func runToolCallErr(t *testing.T, tools map[string]googletool.Metadata[errCapResponse], toolName string, args map[string]any, opts ...googleexecutor.Option[errCapRequest, errCapResponse]) (*bytes.Buffer, error) {
	t.Helper()

	var mu sync.Mutex
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, functionCallTurnJSON(t, "call_g1", toolName, args))
			return
		}
		// submitTurnJSON is shared with validating_server_test.go.
		_, _ = io.WriteString(w, submitTurnJSON)
	}))
	t.Cleanup(srv.Close)

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	opts = append([]googleexecutor.Option[errCapRequest, errCapResponse]{
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (googletool.SubmitMetadata[errCapResponse], error) {
			return googletool.SubmitMetadata[errCapResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_result"},
				Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
					answer, _ := call.Args["answer"].(string)
					return toolcall.SubmitOutcome[errCapResponse]{
						Accepted:   true,
						Response:   errCapResponse{Answer: answer},
						ToolResult: map[string]any{"success": true},
					}
				},
			}, nil
		}),
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		// Context caching is orthogonal here and would need a faked
		// Caches.Create endpoint.
		googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
	}, opts...)
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](newTestClient(t, srv.URL), prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logs := &bytes.Buffer{}
	ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(logs, nil)))
	_, err = exec.Execute(ctx, errCapRequest{}, tools)
	return logs, err
}

// toolCallRecord returns the one "Executing tool call" record for tool, failing
// if there is not exactly one — otherwise every assertion could pass vacuously.
func toolCallRecord(t *testing.T, logs *bytes.Buffer, tool string) map[string]any {
	t.Helper()

	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "Executing tool call" && rec["tool"] == tool {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%q tool-call log records: got = %d, want = 1; every assertion on this record would otherwise prove nothing.\nlogged:\n%s",
			tool, len(found), logs.String())
	}
	return found[0]
}

// assertNameAbsentOutsideTrace fails if secret appears in any log record other
// than the agent tracer's own dump. Scanning every record is the point: the
// executor names a call on several lines, not just "Executing tool call".
//
// The tracer's record is excluded because it serialises the whole trace, tool
// names included, by design. That is the separately-owned leak site this
// change does not cover, and the trace is where an operator reads the real
// name back.
func assertNameAbsentOutsideTrace(t *testing.T, logs *bytes.Buffer, secret string) {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "Agent trace completed" {
			continue
		}
		if strings.Contains(line, secret) {
			t.Errorf("log record %q carries the invented tool name:\n%s", rec["msg"], line)
		}
	}
}

// The per-executor guard. googleexecutor repeats the two lines of argument-log
// wiring rather than sharing them, and metaagent picks the backend by model id,
// so a run that moves to Gemini must withhold what the claude path withholds.
func TestToolCallLog_PolicyWithholdsSensitiveArgs(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]googletool.Metadata[errCapResponse]{
			"git_grep": gitGrepTool(toolcall.NewArgLogPolicy("ref")),
		},
		"git_grep",
		map[string]any{
			"ref":       publicRef,
			"pattern":   sensitivePattern,
			"reasoning": sensitiveReasoning,
		})

	rec := toolCallRecord(t, logs, "git_grep")

	// The allowlisted argument must survive. Without this the test would also
	// pass if the executor stopped logging arguments altogether.
	if got := rec["args.ref"]; got != publicRef {
		t.Errorf("args.ref: got = %v, want = %q (the allowlisted argument must stay legible)", got, publicRef)
	}
	for _, arg := range []string{"args.pattern", "args.reasoning"} {
		if _, ok := rec[arg]; ok {
			t.Errorf("%s: present; a withheld argument contributes no key of its own", arg)
		}
	}
	if got := rec["args_withheld"]; got != float64(2) {
		t.Errorf("args_withheld: got = %v, want = 2", got)
	}
	for key, value := range rec {
		for _, secret := range []string{sensitivePattern, sensitiveReasoning} {
			if strings.Contains(key, secret) {
				t.Errorf("log key %q leaked %q to the log sink", key, secret)
			}
			if text, ok := value.(string); ok && strings.Contains(text, secret) {
				t.Errorf("%s = %q leaked %q to the log sink", key, text, secret)
			}
		}
	}
}

// The tool NAME channel on this backend. genai reports the name the model
// emitted, so a model that invents a function spells its text into the "tool"
// key. A policed run logs a name the run never registered as "[unknown]".
func TestToolCallLog_InventedToolNameIsWithheld(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]googletool.Metadata[errCapResponse]{
			"git_grep": gitGrepTool(toolcall.NewArgLogPolicy("ref")),
		},
		sensitiveReasoning,
		map[string]any{"ref": publicRef})

	// An invented name is an unknown function by definition, so the dispatch
	// branch logs it again one line later, and this backend also names every
	// call while parsing the response parts. Both are scanned here.
	assertNameAbsentOutsideTrace(t, logs, sensitiveReasoning)

	// The record is keyed on the replacement, which fails if the raw name rode
	// through: the emitted name would then be the record's "tool" value.
	rec := toolCallRecord(t, logs, "[unknown]")
	// An unregistered name matches no policy, so the call's own arguments are
	// withheld too rather than riding the unpoliced path.
	if got := rec["args_withheld"]; got != float64(1) {
		t.Errorf("args_withheld: got = %v, want = 1", got)
	}
}

// submitArgs is a submission whose values carry the caller's input text.
var submitArgs = map[string]any{
	"answer":    sensitivePattern,
	"reasoning": sensitiveReasoning,
}

// A run with no tools of its own serves only its submit tool. Without
// WithToolArgsWithheld it logs the submitted arguments, which is every
// driftlessaf agent that has not set the option.
func TestToolCallLog_RunWithNoToolsLogsSubmitArgs(t *testing.T) {
	t.Parallel()

	rec := toolCallRecord(t, runToolCall(t, nil, "submit_result", submitArgs), "submit_result")

	if got := rec["args.answer"]; got != sensitivePattern {
		t.Errorf("args.answer: got = %v, want = %q (a run without the option must log arguments as before)", got, sensitivePattern)
	}
	if _, ok := rec["args_withheld"]; ok {
		t.Errorf("args_withheld: present = %v; a run without the option withholds nothing", rec["args_withheld"])
	}
}

// WithToolArgsWithheld polices a run in which no tool declares a policy, so the
// submit tool keeps its name and its arguments are counted.
func TestToolCallLog_ToolArgsWithheldWithholdsSubmitArgs(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t, nil, "submit_result", submitArgs, googleexecutor.WithToolArgsWithheld[errCapRequest, errCapResponse]())
	rec := toolCallRecord(t, logs, "submit_result")

	for key := range rec {
		if strings.HasPrefix(key, "args.") {
			t.Errorf("%s: present; WithToolArgsWithheld logs no argument that no policy allows", key)
		}
	}
	if got := rec["args_withheld"]; got != float64(2) {
		t.Errorf("args_withheld: got = %v, want = 2", got)
	}
	assertNameAbsentOutsideTrace(t, logs, sensitivePattern)
	assertNameAbsentOutsideTrace(t, logs, sensitiveReasoning)
}

// Under WithToolArgsWithheld a name the run never registered is model text, so
// it is replaced even though no tool declares a policy.
func TestToolCallLog_ToolArgsWithheldReplacesInventedName(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t, nil, sensitiveReasoning, map[string]any{"ref": publicRef}, googleexecutor.WithToolArgsWithheld[errCapRequest, errCapResponse]())

	assertNameAbsentOutsideTrace(t, logs, sensitiveReasoning)
	if got := toolCallRecord(t, logs, "[unknown]")["args_withheld"]; got != float64(1) {
		t.Errorf("args_withheld: got = %v, want = 1", got)
	}
}

// A run whose tools declare policies is already policed, so the option leaves
// the allowlisted argument legible and withholds the rest.
func TestToolCallLog_ToolArgsWithheldKeepsPolicyAllowedArgs(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]googletool.Metadata[errCapResponse]{
			"git_grep": gitGrepTool(toolcall.NewArgLogPolicy("ref")),
		},
		"git_grep",
		map[string]any{
			"ref":       publicRef,
			"pattern":   sensitivePattern,
			"reasoning": sensitiveReasoning,
		},
		googleexecutor.WithToolArgsWithheld[errCapRequest, errCapResponse]())

	rec := toolCallRecord(t, logs, "git_grep")
	if got := rec["args.ref"]; got != publicRef {
		t.Errorf("args.ref: got = %v, want = %q", got, publicRef)
	}
	if got := rec["args_withheld"]; got != float64(2) {
		t.Errorf("args_withheld: got = %v, want = 2", got)
	}
	assertNameAbsentOutsideTrace(t, logs, sensitivePattern)
	assertNameAbsentOutsideTrace(t, logs, sensitiveReasoning)
}

// The suspend tool is held out of the tool map like the submit tool, so under
// WithToolArgsWithheld it must still be logged by name, not as "[unknown]",
// with its arguments counted rather than logged.
func TestToolCallLog_ToolArgsWithheldWithholdsSuspendArgs(t *testing.T) {
	t.Parallel()

	logs, err := runToolCallErr(t, nil, "ask",
		map[string]any{"question": sensitivePattern, "context": sensitiveReasoning},
		googleexecutor.WithSuspendTool[errCapRequest, errCapResponse](askTool),
		googleexecutor.WithToolArgsWithheld[errCapRequest, errCapResponse]())
	if _, ok := checkpoint.AsSuspension(err); !ok {
		t.Fatalf("Execute: got err = %v, want a *checkpoint.Suspension", err)
	}

	rec := toolCallRecord(t, logs, "ask")
	for key := range rec {
		if strings.HasPrefix(key, "args.") {
			t.Errorf("%s: present; WithToolArgsWithheld logs no argument that no policy allows", key)
		}
	}
	if got := rec["args_withheld"]; got != float64(2) {
		t.Errorf("args_withheld: got = %v, want = 2", got)
	}
	assertNameAbsentOutsideTrace(t, logs, sensitivePattern)
	assertNameAbsentOutsideTrace(t, logs, sensitiveReasoning)
}
