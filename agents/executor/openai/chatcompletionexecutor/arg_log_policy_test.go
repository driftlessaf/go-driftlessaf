/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package chatcompletionexecutor_test

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
	"chainguard.dev/driftlessaf/agents/executor/openai/chatcompletionexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// Stand-ins for confidential caller input: a tool argument holding a string
// from the caller's own input, and a model-written justification repeating it.
// A neutral token would not exercise the paraphrase case the policy exists for.
const (
	sensitivePattern   = "restricted-input-7742"
	sensitiveReasoning = "Confirm 4.1.0 still references restricted-input-7742 in lib/index.ts."
	publicRef          = "4.1.0"
)

// namedToolCompletionJSON renders a completion calling toolName with args. The
// executor emits its "Executing tool call" line before it dispatches, so this
// drives the line whether or not a handler is registered for the name.
func namedToolCompletionJSON(t *testing.T, id, callID, toolName string, args map[string]any) string {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	// The OpenAI wire format carries the arguments as a JSON *string*.
	quoted, err := json.Marshal(string(argsJSON))
	if err != nil {
		t.Fatalf("marshal arguments string: %v", err)
	}
	return fmt.Sprintf(`{
  "id": %q,
  "object": "chat.completion",
  "created": 1,
  "model": "test-model",
  "choices": [{
    "index": 0,
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "content": "",
      "tool_calls": [
        {"id": %q, "type": "function", "function": {"name": %q, "arguments": %s}}
      ]
    }
  }],
  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
}`, id, callID, toolName, quoted)
}

// gitGrepTool builds a git_grep tool through the real openaistool.FromTool, so a
// policy that registration declares but the conversion drops fails these tests.
func gitGrepTool(policy *toolcall.ArgLogPolicy) openaistool.Metadata[errCapResponse] {
	return openaistool.FromTool(toolcall.Tool[errCapResponse]{
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

// runToolCall drives a real executor through one call to toolName with args,
// then a submit, and returns everything written to the context's log sink.
func runToolCall(t *testing.T, tools map[string]openaistool.Metadata[errCapResponse], toolName string, args map[string]any) *bytes.Buffer {
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
			_, _ = io.WriteString(w, namedToolCompletionJSON(t, "chatcmpl-1", "call_g1", toolName, args))
			return
		}
		_, _ = io.WriteString(w, submitCompletionJSON(t, "chatcmpl-2", "call_s1", "done"))
	}))
	t.Cleanup(srv.Close)

	client := openai.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := chatcompletionexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		chatcompletionexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.OpenAIToolForResponse[errCapResponse]),
		chatcompletionexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		chatcompletionexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logs := &bytes.Buffer{}
	ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(logs, nil)))
	if _, err := exec.Execute(ctx, errCapRequest{}, tools); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return logs
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

// The per-executor guard. This executor repeats the two lines of argument-log
// wiring rather than sharing them, and metaagent picks the backend by model id,
// so a run that moves to an OpenAI-compatible gateway must withhold what the
// claude path withholds.
func TestToolCallLog_PolicyWithholdsSensitiveArgs(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]openaistool.Metadata[errCapResponse]{
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

// The tool NAME channel on this backend. The provider reports the name the
// model emitted, so a model that invents a function spells its text into the
// "tool" key. A policed run logs a name it never registered as "[unknown]".
func TestToolCallLog_InventedToolNameIsWithheld(t *testing.T) {
	t.Parallel()

	logs := runToolCall(t,
		map[string]openaistool.Metadata[errCapResponse]{
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
