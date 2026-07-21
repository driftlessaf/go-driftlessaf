/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultResumeModel is the executor's default model id (claudeexecutor.New
// leaves it unset-to-default). A resume envelope must carry the same model or
// the fail-closed check rejects it, so the fixtures below stamp this value.
const defaultResumeModel = "claude-sonnet-4@20250514"

// liveConfigDigest reproduces the executor's turn-invariant static params for
// the resume-test configuration — default model, default 8192 max tokens, no
// tools (empty tool list), default 0.1 temperature, no system prompt — and
// digests them exactly as buildSuspension/Resume do. Resume now validates the
// envelope's ConfigDigest unconditionally (an empty digest no longer skips the
// check), so the fixture must carry the matching value.
func liveConfigDigest(t *testing.T) string {
	t.Helper()
	digest, err := checkpoint.DigestJSON(anthropic.MessageNewParams{
		Model:       defaultResumeModel,
		MaxTokens:   8192,
		Tools:       []anthropic.ToolUnionParam{},
		Temperature: anthropic.Float(0.1),
	})
	if err != nil {
		t.Fatalf("DigestJSON: %v", err)
	}
	return digest
}

// staleCacheProviderState builds a captured MessageNewParams that carries stale
// cache_control markers on FIVE blocks — one tool, the system block, and three
// message blocks — so a verbatim replay would exceed the four-breakpoint API
// limit. The assistant turn issues the pending suspend tool_use "toolu_ask"; the
// trailing empty user message is the slot the framed human answer pairs into on
// resume. It returns the marshaled JSON and the number of cache_control markers
// present (the verbatim-replay count).
func staleCacheProviderState(t *testing.T) (json.RawMessage, int) {
	t.Helper()

	toolBlock := anthropic.NewTextBlock("please decide")
	toolBlock.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()

	askUse := anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{
			ID:    "toolu_ask",
			Name:  askAFriendToolName,
			Input: json.RawMessage(`{"question":"ship it?"}`),
		},
	}
	askUse.OfToolUse.CacheControl = anthropic.NewCacheControlEphemeralParam()

	params := anthropic.MessageNewParams{
		Model:     defaultResumeModel,
		MaxTokens: 8192,
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name: "lookup",
			InputSchema: anthropic.ToolInputSchemaParam{
				Type:       "object",
				Properties: map[string]any{"reasoning": map[string]any{"type": "string"}},
			},
			CacheControl: anthropic.NewCacheControlEphemeralParam(), // marker 1 (tool)
		}}},
		System: []anthropic.TextBlockParam{{
			Text:         "system instructions",
			CacheControl: anthropic.NewCacheControlEphemeralParam(), // marker 2 (system)
		}},
		Messages: []anthropic.MessageParam{
			{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{toolBlock}},   // marker 3 (first user block)
			{Role: anthropic.MessageParamRoleAssistant, Content: []anthropic.ContentBlockParamUnion{askUse}}, // marker 4 (tool_use)
			{Role: anthropic.MessageParamRoleUser, Content: nil},                                             // answer slot
		},
	}
	// A fifth marker on the assistant text keeps the verbatim count above four
	// even after the pending tool_use pairs.
	thinkBlock := anthropic.NewTextBlock("thinking about it")
	thinkBlock.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam() // marker 5
	params.Messages[1].Content = append([]anthropic.ContentBlockParamUnion{thinkBlock}, params.Messages[1].Content...)

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal provider state: %v", err)
	}
	markers := strings.Count(string(raw), `"cache_control"`)
	return raw, markers
}

// TestResumeStripsStaleCacheControl is the PR6 key case: a checkpoint whose
// transcript carries stale cache_control markers, when resumed, produces a
// request with <= 4 markers — and a verbatim replay (no strip) would have
// exceeded 4. The validating server also asserts the tool_use/tool_result
// pairing the injected answer completes.
func TestResumeStripsStaleCacheControl(t *testing.T) {
	providerState, verbatimMarkers := staleCacheProviderState(t)
	if verbatimMarkers <= validatingServerMaxCacheBreakpoints {
		t.Fatalf("fixture invariant: verbatim markers = %d, want > %d so a no-strip replay would 400",
			verbatimMarkers, validatingServerMaxCacheBreakpoints)
	}

	// The resumed run's first (and only) turn answers with plain text, which the
	// no-submit-tool executor parses as the final result.
	finalTurn := []string{
		`{"type":"message_start","message":{"id":"msg_resume","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":30,"output_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"answer\":\"resumed\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}

	var firstBody []byte
	srv := newValidatingAnthropicServer(t, func(reqNum int, body []byte) []string {
		if reqNum == 1 {
			firstBody = append([]byte(nil), body...)
		}
		return finalTurn
	})

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](7),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resumer, ok := exec.(claudeexecutor.Resumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor does not satisfy Resumer")
	}

	env := checkpoint.Envelope{
		Version:        checkpoint.EnvelopeVersion,
		Provider:       "anthropic",
		Model:          defaultResumeModel,
		ConfigDigest:   liveConfigDigest(t),
		Turn:           0,
		RemainingTurns: 5,
		Reason:         "awaiting answer",
		PendingToolCalls: []checkpoint.PendingToolCall{{
			ID:   "toolu_ask",
			Name: askAFriendToolName,
		}},
		ProviderState: providerState,
		TraceID:       "trace-origin",
	}

	answers := map[string]string{"toolu_ask": "yes, ship it"}
	resp, err := resumer.Resume(t.Context(), env, answers, map[string]claudetool.Metadata[errCapResponse]{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, want := resp.Answer, "resumed"; got != want {
		t.Errorf("resp.Answer: got %q, want %q", got, want)
	}

	if firstBody == nil {
		t.Fatal("validating server never received the resumed request")
	}
	actualMarkers := strings.Count(string(firstBody), `"cache_control"`)
	if actualMarkers > validatingServerMaxCacheBreakpoints {
		t.Errorf("resumed request carried %d cache_control markers, exceeds the API limit of %d",
			actualMarkers, validatingServerMaxCacheBreakpoints)
	}
	if actualMarkers >= verbatimMarkers {
		t.Errorf("strip was a no-op: resumed markers = %d, verbatim = %d (want resumed < verbatim)",
			actualMarkers, verbatimMarkers)
	}
	// The framed human answer must be present as the pending call's tool_result,
	// completing the tool_use/tool_result pairing.
	if !strings.Contains(string(firstBody), "BEGIN HUMAN ANSWER") {
		t.Errorf("resumed request does not carry the framed human answer: %s", firstBody)
	}
}

// TestResumeCapsTurnBudgetAtLiveMaxTurns pins the resume-side turn cap: an
// envelope whose RemainingTurns exceeds the live executor's configured
// maxTurns resumes with at most the live budget. maxTurns is loop config, not
// part of the request digest, so ValidateForResume cannot catch the mismatch —
// without the call-site cap, a checkpoint parked under a larger budget (an
// operator lowering maxTurns between park and wake, or a tampered envelope)
// would grant the resumed run more turns than the live configuration allows.
func TestResumeCapsTurnBudgetAtLiveMaxTurns(t *testing.T) {
	providerState, _ := staleCacheProviderState(t)

	// Every turn calls a tool that is not registered with the resumed run, so
	// each turn produces an unknown-tool result and the loop continues — the run
	// can only end by exhausting its turn budget.
	var requests int
	srv := newValidatingAnthropicServer(t, func(reqNum int, _ []byte) []string {
		requests = reqNum
		return wrapTurn(fmt.Sprintf("msg_loop_%d", reqNum),
			toolUseBlockSSE(t, 0, fmt.Sprintf("toolu_loop_%d", reqNum), "lookup",
				map[string]any{"reasoning": "keep digging"})...)
	})

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	const liveMaxTurns = 1
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](liveMaxTurns),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resumer, ok := exec.(claudeexecutor.Resumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor does not satisfy Resumer")
	}

	env := checkpoint.Envelope{
		Version:        checkpoint.EnvelopeVersion,
		Provider:       "anthropic",
		Model:          defaultResumeModel,
		ConfigDigest:   liveConfigDigest(t),
		Turn:           0,
		RemainingTurns: 5, // exceeds the live executor's budget of 1
		Reason:         "awaiting answer",
		PendingToolCalls: []checkpoint.PendingToolCall{{
			ID:   "toolu_ask",
			Name: askAFriendToolName,
		}},
		ProviderState: providerState,
	}

	_, err = resumer.Resume(t.Context(), env, map[string]string{"toolu_ask": "yes"},
		map[string]claudetool.Metadata[errCapResponse]{})
	if err == nil {
		t.Fatal("Resume: got nil error, want turn-budget exhaustion")
	}
	if want := fmt.Sprintf("exceeded maximum conversation turns (%d)", liveMaxTurns); !strings.Contains(err.Error(), want) {
		t.Errorf("Resume error: got %q, want it to contain %q (the capped budget, not the envelope's)", err.Error(), want)
	}
	if requests != liveMaxTurns {
		t.Errorf("provider requests: got %d, want %d (envelope's RemainingTurns must not override the live maxTurns)", requests, liveMaxTurns)
	}
}

// TestResumeFailsClosedOnModelDrift proves the fail-closed check: an envelope
// whose model no longer matches the live executor is rejected with
// checkpoint.ErrConfigDrift rather than resumed against stale state.
func TestResumeFailsClosedOnModelDrift(t *testing.T) {
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		anthropic.NewClient(option.WithAPIKey("test")),
		prompt,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resumer := exec.(claudeexecutor.Resumer[errCapRequest, errCapResponse])

	env := checkpoint.Envelope{
		Version:          checkpoint.EnvelopeVersion,
		Provider:         "anthropic",
		Model:            "claude-opus-4-8", // differs from the executor default
		RemainingTurns:   5,
		PendingToolCalls: []checkpoint.PendingToolCall{{ID: "toolu_ask", Name: askAFriendToolName}},
		ProviderState:    json.RawMessage(`{"model":"claude-opus-4-8"}`),
	}

	_, err = resumer.Resume(t.Context(), env, nil, map[string]claudetool.Metadata[errCapResponse]{})
	if err == nil {
		t.Fatal("Resume: got nil error, want ErrConfigDrift on model mismatch")
	}
	if !errors.Is(err, checkpoint.ErrConfigDrift) {
		t.Errorf("Resume error: got %v, want checkpoint.ErrConfigDrift", err)
	}
}
