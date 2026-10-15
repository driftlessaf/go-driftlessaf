/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
)

// writeClaudeSubmitTurn streams one assistant turn that submits answer.
func writeClaudeSubmitTurn(t *testing.T, w http.ResponseWriter, model, answer string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	args := fmt.Sprintf(`{"reasoning":"complete","result":{"answer":%q}}`, answer)
	for _, event := range []map[string]any{
		{"type": "message_start", "message": map[string]any{
			"id": "msg-1", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 1},
		}},
		{"type": "content_block_start", "index": 0, "content_block": map[string]any{
			"type": "tool_use", "id": "tool-1", "name": "submit_result", "input": map[string]any{},
		}},
		{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 3}},
		{"type": "message_stop"},
	} {
		data, err := json.Marshal(event)
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}

// TestNewWithTargetAppliesTargetMaxTokens builds a Bedrock agent from a
// target and checks the output cap each request sends.
func TestNewWithTargetAppliesTargetMaxTokens(t *testing.T) {
	t.Parallel()
	route := runtimeClaudeRoute()
	for _, tc := range []struct {
		name      string
		targetCap int64
		configCap int64
		want      int64
	}{
		{name: "neither sets a cap", want: defaultMaxTokens},
		{name: "target cap", targetCap: 16000, want: 16000},
		{name: "agent cap wins over the target cap", targetCap: 16000, configCap: 256, want: 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []float64
			adapter := runtimeClaudeAdapter(t, func(w http.ResponseWriter, request *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				maxTokens, _ := body["max_tokens"].(float64)
				got = append(got, maxTokens)
				writeClaudeSubmitTurn(t, w, route.ProviderModelID, "done")
			})
			runtime, err := NewRuntime([]modelrouter.Route{route}, NewAdapterBackend("", modelrouter.ProviderAWSBedrock, func(string) (AdapterRegistrations, error) {
				return AdapterRegistrations{AnthropicMessages: []AnthropicMessagesRegistration{{Provider: modelrouter.ProviderAWSBedrock, Adapter: adapter}}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			cfg := routedTestConfig(t)
			cfg.MaxTokens = tc.configCap
			target := runtime.Target(TargetConfig{Provider: modelrouter.ProviderAWSBedrock, Model: route.Selection.LogicalModel, Region: "us-west-2", MaxTokens: tc.targetCap})
			if got := target.MaxTokens(); got != tc.targetCap {
				t.Errorf("target cap: got = %d, want = %d", got, tc.targetCap)
			}
			agent, err := NewWithTarget[*testRequest](t.Context(), target, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := agent.Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{}); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != float64(tc.want) {
				t.Errorf("output caps sent: got = %v, want = [%d]", got, tc.want)
			}
		})
	}
}

func TestTargetRejectsNegativeMaxTokens(t *testing.T) {
	t.Parallel()
	route := runtimeClaudeRoute()
	runtime, err := NewRuntime([]modelrouter.Route{route}, NewAdapterBackend("", modelrouter.ProviderAWSBedrock, func(string) (AdapterRegistrations, error) {
		t.Error("adapter factory invoked for an invalid target")
		return AdapterRegistrations{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	target := runtime.Target(TargetConfig{Provider: modelrouter.ProviderAWSBedrock, Model: route.Selection.LogicalModel, Region: "us-west-2", MaxTokens: -1})
	if _, _, err := target.Resolve(); err == nil || !strings.Contains(err.Error(), "max tokens must not be negative") {
		t.Errorf("Resolve error: got = %v, want a negative cap error", err)
	}
	if _, err := NewWithTarget[*testRequest](t.Context(), target, routedTestConfig(t)); err == nil {
		t.Error("NewWithTarget succeeded with a negative target cap")
	}
}

// TestRuntimeClaudeThrottlingReachesRetry sends a Bedrock Runtime throttling
// response through the Bedrock Anthropic Messages adapter. The Claude
// executor must classify it as retryable and send the turn again.
func TestRuntimeClaudeThrottlingReachesRetry(t *testing.T) {
	t.Parallel()
	route := runtimeClaudeRoute()
	calls := 0
	adapter := runtimeClaudeAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Amzn-Errortype", "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"Too many requests, please wait before trying again."}`)
			return
		}
		writeClaudeSubmitTurn(t, w, route.ProviderModelID, "done")
	})
	binding, err := adapter(t.Context(), mustClaudePlan(t, route))
	if err != nil {
		t.Fatal(err)
	}
	cfg := routedTestConfig(t)
	submitTool, err := submitresult.ClaudeTool(submitOptions(cfg))
	if err != nil {
		t.Fatal(err)
	}
	executor, err := claudeexecutor.NewWithMessages[*testRequest, *basetenTestResponse](binding.Messages(), cfg.UserPrompt,
		claudeexecutor.WithRoutedModel[*testRequest, *basetenTestResponse](route.ProviderModelID, route.Selection.LogicalModel),
		claudeexecutor.WithSubmitResultProvider[*testRequest, *basetenTestResponse](func() (claudetool.SubmitMetadata[*basetenTestResponse], error) {
			return submitTool, nil
		}),
		claudeexecutor.WithRetryConfig[*testRequest, *basetenTestResponse](retry.RetryConfig{
			MaxRetries: 1, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), &testRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Answer != "done" || calls != 2 {
		t.Errorf("result/attempts: got = %q/%d, want = done/2", result.Answer, calls)
	}
}
