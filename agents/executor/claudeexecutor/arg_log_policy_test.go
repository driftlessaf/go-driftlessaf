/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

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
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/chainguard-dev/clog"
)

// Stand-ins for confidential caller input: a tool argument holding a string
// from the caller's own input, and a model-written justification repeating it.
// A neutral token would not exercise the paraphrase case the policy exists for.
const (
	sensitivePattern   = "restricted-input-7742"
	sensitiveReasoning = "Confirm 4.1.0 still references restricted-input-7742 in lib/index.ts."
	sensitivePath      = "lib/index.ts"
	publicRef          = "4.1.0"
)

// namedToolCallTurn renders an assistant turn calling a REGISTERED tool, which
// is the dispatch branch that emits the "Executing tool call" line under test.
func namedToolCallTurn(t *testing.T, msgID, callID, toolName string, input map[string]any) []string {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	partial, err := json.Marshal(string(inputJSON))
	if err != nil {
		t.Fatalf("marshal partial: %v", err)
	}
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`, msgID),
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, callID, toolName),
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
}

// gitGrepTool builds a git_grep tool through the real claudetool.FromTool, so a
// policy that registration declares but the conversion drops fails these tests.
func gitGrepTool(policy *toolcall.ArgLogPolicy) claudetool.Metadata[errCapResponse] {
	return claudetool.FromTool(toolcall.Tool[errCapResponse]{
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

// runToolCall drives a real executor through one call to toolName with input,
// then a submit, and returns everything written to the context's log sink.
func runToolCall(t *testing.T, tools map[string]claudetool.Metadata[errCapResponse], toolName string, input map[string]any) *bytes.Buffer {
	t.Helper()

	var mu sync.Mutex
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()

		events := namedToolCallTurn(t, "msg_01", "toolu_g1", toolName, input)
		if n > 1 {
			events = submitCallTurn(t, "msg_02", "toolu_s1", submitInput("done"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)

	logs := &bytes.Buffer{}
	ctx := clogJSON(t, logs)
	if _, err := newSubmitExecutor(t, srv).Execute(ctx, errCapRequest{}, tools); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return logs
}

// toolCallRecord returns the one "Executing tool call" record for tool, failing
// if there is not exactly one — otherwise every assertion could pass vacuously.
// It reads that record alone: the buffer also carries the trace-completion line.
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

// assertAbsent fails if any key OR value carries a sensitive string. Keys count:
// the executor builds them from the model's JSON, so a key is model-controlled.
func assertAbsent(t *testing.T, rec map[string]any, secrets ...string) {
	t.Helper()
	for key, value := range rec {
		for _, secret := range secrets {
			if strings.Contains(key, secret) {
				t.Errorf("log key %q leaked %q to the log sink", key, secret)
			}
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s = %q leaked %q to the log sink", key, text, secret)
			}
		}
	}
}

// The regression guard: the executor used to log every argument it could
// unmarshal, so the caller's pattern and the injected reasoning both reached the
// sink. With a policy naming only `ref`, the ref stays and the others do not.
func TestToolCallLog_PolicyWithholdsSensitiveArgs(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]claudetool.Metadata[errCapResponse]{
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
	// pass if the executor stopped logging arguments altogether, which is a
	// different change from the one being pinned.
	if got := rec["args.ref"]; got != publicRef {
		t.Errorf("args.ref: got = %v, want = %q (the allowlisted argument must stay legible)", got, publicRef)
	}
	// The two withheld arguments are counted, not named: their names come from
	// the model's JSON, so the key would carry whatever the model wrote there.
	for _, arg := range []string{"args.pattern", "args.reasoning"} {
		if _, ok := rec[arg]; ok {
			t.Errorf("%s: present; a withheld argument contributes no key of its own", arg)
		}
	}
	if got := rec["args_withheld"]; got != float64(2) {
		t.Errorf("args_withheld: got = %v, want = 2", got)
	}
	assertAbsent(t, rec, sensitivePattern, sensitiveReasoning)
}

// Fail-closed default: the policy names `ref` only, and the model supplies a
// `path` it never mentions — the shape of an argument added to the tool later.
func TestToolCallLog_UndeclaredArgIsWithheld(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]claudetool.Metadata[errCapResponse]{
			"git_grep": gitGrepTool(toolcall.NewArgLogPolicy("ref")),
		},
		"git_grep",
		map[string]any{
			"ref":       publicRef,
			"pattern":   sensitivePattern,
			"path":      sensitivePath,
			"reasoning": sensitiveReasoning,
		})

	rec := toolCallRecord(t, logs, "git_grep")

	// An argument the policy does not name is withheld whole — no key, no value.
	if _, ok := rec["args.path"]; ok {
		t.Error("args.path: present; an argument the policy does not name must contribute no key")
	}
	// It is still counted, so the line reports that arguments were supplied.
	if got := rec["args_withheld"]; got != float64(3) {
		t.Errorf("args_withheld: got = %v, want = 3 (pattern, path, reasoning)", got)
	}
	assertAbsent(t, rec, sensitivePattern, sensitiveReasoning, sensitivePath)
}

// The run-level half of the default: one tool declaring a policy policies every
// tool of the run, so a sibling that declared none cannot leak.
func TestToolCallLog_UnpolicedToolInPolicedRunIsWithheld(t *testing.T) {
	t.Parallel()

	unpoliced := claudetool.FromTool(toolcall.Tool[errCapResponse]{
		Def: toolcall.Definition{
			Name:        "git_log_search",
			Description: "Search history for a pattern.",
			Parameters: []toolcall.Parameter{
				{Name: "pattern", Type: "string", Description: "The text to search for.", Required: true},
			},
			// No LogPolicy on purpose: this is the tool someone adds later.
		},
		Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
			return map[string]any{"log": ""}
		},
	})

	logs := runToolCall(t,
		map[string]claudetool.Metadata[errCapResponse]{
			"git_grep":       gitGrepTool(toolcall.NewArgLogPolicy("ref")),
			"git_log_search": unpoliced,
		},
		"git_log_search",
		map[string]any{
			"pattern":   sensitivePattern,
			"reasoning": sensitiveReasoning,
		})

	rec := toolCallRecord(t, logs, "git_log_search")

	for _, arg := range []string{"args.pattern", "args.reasoning"} {
		if _, ok := rec[arg]; ok {
			t.Errorf("%s: present; a tool that declared no policy is withheld in a policed run", arg)
		}
	}
	if got := rec["args_withheld"]; got != float64(2) {
		t.Errorf("args_withheld: got = %v, want = 2", got)
	}
	assertAbsent(t, rec, sensitivePattern, sensitiveReasoning)
}

// Blast radius: an agent where no tool declares a policy logs every argument
// exactly as before, which is every driftlessaf agent other than these two.
func TestToolCallLog_UnpolicedRunLogsEveryArg(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]claudetool.Metadata[errCapResponse]{
			"git_grep": gitGrepTool(nil),
		},
		"git_grep",
		map[string]any{
			"ref":       publicRef,
			"pattern":   "findByName",
			"reasoning": "locate the accessor",
		})

	rec := toolCallRecord(t, logs, "git_grep")

	for arg, want := range map[string]string{
		"args.ref":       publicRef,
		"args.pattern":   "findByName",
		"args.reasoning": "locate the accessor",
	} {
		if got := rec[arg]; got != want {
			t.Errorf("%s: got = %v, want = %q (an unpoliced run must log arguments as before)", arg, got, want)
		}
	}
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

// The tool NAME channel. The provider reports the name the model emitted, so a
// model that invents a function spells its text into the "tool" key. A policed
// run logs a name it never registered as "[unknown]".
func TestToolCallLog_InventedToolNameIsWithheld(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]claudetool.Metadata[errCapResponse]{
			"git_grep": gitGrepTool(toolcall.NewArgLogPolicy("ref")),
		},
		sensitiveReasoning,
		map[string]any{"ref": publicRef})

	// An invented name is an unknown tool by definition, so the dispatch branch
	// logs it again one line later. Checking the "Executing tool call" record
	// alone would pass while that ERROR line carries the name.
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

// The cut-off call takes its own path: the output cap ends the turn before the
// call is dispatched, so it never reaches "Executing tool call" and is named by
// the max_tokens warning instead. The name is still the model's.
func TestTruncatedToolCallLog_InventedToolNameIsWithheld(t *testing.T) {
	t.Parallel()

	client, _ := newExecTestClient(t, func(n int) []string {
		if n == 1 {
			// Cut mid input, which is what makes the call truncated.
			return toolCallTurn("msg_c1", "toolu_c1", sensitiveReasoning, `{"ref":"4.1.0`, "max_tokens", 32000)
		}
		return answerTurn
	})

	logs := &bytes.Buffer{}
	ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(logs, nil)))
	tools := map[string]claudetool.Metadata[errCapResponse]{
		"git_grep": gitGrepTool(toolcall.NewArgLogPolicy("ref")),
	}
	if _, err := newTruncationExecutor(t, client, 3).Execute(ctx, errCapRequest{}, tools); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertNameAbsentOutsideTrace(t, logs, sensitiveReasoning)
}

// clogJSON returns a context whose logger writes JSON records to w, so a test
// can read the executor's own log line back field by field.
func clogJSON(t *testing.T, w io.Writer) context.Context {
	t.Helper()
	return clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(w, nil)))
}

// The argument NAME channel: the log key comes from the model's JSON, so an
// invented argument puts the model's text in the key even when its value is
// withheld. A policed run logs a name only when the policy allows it.
func TestToolCallLog_InventedArgumentNameIsWithheld(t *testing.T) {
	t.Parallel()

	// The model invents an argument whose NAME carries the sensitive text. Its
	// value is deliberately innocuous, so only the key can fail this test.
	logs := runToolCall(t,
		map[string]claudetool.Metadata[errCapResponse]{
			"git_grep": gitGrepTool(toolcall.NewArgLogPolicy("ref")),
		},
		"git_grep",
		map[string]any{
			"ref":            publicRef,
			sensitivePattern: "1",
		})

	rec := toolCallRecord(t, logs, "git_grep")

	// The allowlisted argument still appears by name, so the line keeps its
	// value to an operator.
	if got := rec["args.ref"]; got != publicRef {
		t.Errorf("args.ref: got = %v, want = %q", got, publicRef)
	}
	// The invented name must not appear as a key at all.
	if _, ok := rec["args."+sensitivePattern]; ok {
		t.Errorf("the invented argument name reached the log sink as a key.\nrecord: %v", rec)
	}
	// Nor anywhere else in the record, as a key or a value.
	for key, value := range rec {
		if strings.Contains(key, sensitivePattern) {
			t.Errorf("log key %q carries the invented name", key)
		}
		if text, ok := value.(string); ok && strings.Contains(text, sensitivePattern) {
			t.Errorf("log field %s = %q carries the invented name", key, text)
		}
	}
	// The withheld argument is still counted, so the line reports that one was
	// supplied without naming it.
	if got := rec["args_withheld"]; got != float64(1) {
		t.Errorf("args_withheld: got = %v, want = 1", got)
	}
}
