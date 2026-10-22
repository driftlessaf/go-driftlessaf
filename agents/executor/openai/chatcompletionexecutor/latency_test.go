/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package chatcompletionexecutor_test

import (
	"context"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/openai/chatcompletionexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

func TestExecutorRecordsCallLatency(t *testing.T) {
	srv := newValidatingOpenAIServer(t, func(reqNum int, _ []byte) string {
		if reqNum == 1 {
			return turn1ToolCallsJSON
		}
		return turn2SubmitJSON
	})
	client := openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	mkTool := func(name string) openaistool.Metadata[errCapResponse] {
		return openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: name, Description: "test tool " + name},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": name}
			},
		})
	}
	tools := map[string]openaistool.Metadata[errCapResponse]{"tool_a": mkTool("tool_a"), "tool_b": mkTool("tool_b")}
	submit := func() (openaistool.SubmitMetadata[errCapResponse], error) {
		return openaistool.SubmitMetadata[errCapResponse]{
			Definition: openai.ChatCompletionFunctionToolParam{Function: shared.FunctionDefinitionParam{Name: "submit_result"}},
			Handler: func(context.Context, openai.ChatCompletionMessageToolCallUnion, *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
				return toolcall.SubmitOutcome[errCapResponse]{Accepted: true, Response: errCapResponse{Answer: "done"}, ToolResult: map[string]any{"success": true}}
			},
		}, nil
	}
	exec, err := chatcompletionexecutor.New[errCapRequest, errCapResponse](client, prompt,
		chatcompletionexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submit),
		chatcompletionexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		chatcompletionexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracer := &recordingTracer{}
	if _, err := exec.Execute(agenttrace.WithTracer[errCapResponse](t.Context(), tracer), errCapRequest{}, tools); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	turns := tracer.traces[0].Turns
	if len(turns) != 2 {
		t.Fatalf("turns: got = %d, want = 2", len(turns))
	}
	for i, wantReused := range []bool{false, true} {
		l := turns[i].Latency
		if l == nil {
			t.Fatalf("turn %d: Latency = nil, want the answering attempt's timing", i)
		}
		if err := l.Validate(); err != nil {
			t.Errorf("turn %d: Latency.Validate(): %v", i, err)
		}
		if l.RequestStartTime.IsZero() || l.FirstByteMS == nil {
			t.Errorf("turn %d: Latency = %+v, want start time and first byte", i, *l)
		}
		if l.ConnReused == nil || *l.ConnReused != wantReused {
			t.Errorf("turn %d: ConnReused = %v, want %t", i, l.ConnReused, wantReused)
		}
		if got := l.ConnectMS != nil; got == wantReused {
			t.Errorf("turn %d: ConnectMS set = %t, want %t", i, got, !wantReused)
		}
		if l.FirstTokenMS != nil {
			t.Errorf("turn %d: FirstTokenMS = %d, want nil for a call that does not stream", i, *l.FirstTokenMS)
		}
	}
}
