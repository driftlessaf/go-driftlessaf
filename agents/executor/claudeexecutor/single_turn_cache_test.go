/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// wireCacheMarkers counts the cache_control markers a request body carries on
// its system blocks and on its message content blocks.
func wireCacheMarkers(t *testing.T, body []byte) (system, messages int) {
	t.Helper()
	var req struct {
		System []struct {
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Content []struct {
				CacheControl json.RawMessage `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	for _, s := range req.System {
		if len(s.CacheControl) > 0 {
			system++
		}
	}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if len(c.CacheControl) > 0 {
				messages++
			}
		}
	}
	return system, messages
}

// TestSingleTurnRequestOmitsTailCacheMarker pins where the executor places
// cache_control on the wire. A request that advertises no tools, without the
// refusal nudge, ends on the model's first reply, so it carries no marker on
// the prompt (which would be written to the cache and never read); the
// system-prompt marker stays because other executions share that prefix. A
// request that can continue, including one whose only tool is the terminal
// submit tool, keeps the tail marker on its prompt.
func TestSingleTurnRequestOmitsTailCacheMarker(t *testing.T) {
	lookup := map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup"},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": true}
			},
		},
	}

	for _, tc := range []struct {
		name  string
		opts  []claudeexecutor.Option[errCapRequest, errCapResponse]
		tools map[string]claudetool.Metadata[errCapResponse]
		// reply is the model's turn; nil answers in text.
		reply          func(t *testing.T) []string
		wantSystem     int
		wantMessageCCs int
	}{{
		name:           "no tools is single-turn",
		wantSystem:     1,
		wantMessageCCs: 0,
	}, {
		name:           "tools keep the tail marker",
		tools:          lookup,
		wantSystem:     1,
		wantMessageCCs: 1,
	}, {
		name:           "refusal nudge keeps the tail marker",
		opts:           []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithRefusalNudge[errCapRequest, errCapResponse](1)},
		wantSystem:     1,
		wantMessageCCs: 1,
	}, {
		// The submit tool is advertised without any caller tool, and its
		// result validators can send the model back for another turn.
		name: "submit tool alone keeps the tail marker",
		opts: []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse])},
		reply: func(t *testing.T) []string {
			return submitCallTurn(t, "msg_submit", "toolu_submit", submitInput("42"))
		},
		wantSystem:     1,
		wantMessageCCs: 1,
	}, {
		name:           "cache control off places no markers",
		tools:          lookup,
		opts:           []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithoutCacheControl[errCapRequest, errCapResponse]()},
		wantSystem:     0,
		wantMessageCCs: 0,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := newExecTestClient(t, func(int) []string {
				if tc.reply != nil {
					return tc.reply(t)
				}
				return answerTurn
			})

			prompt, err := promptbuilder.NewPrompt("large judged payload")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			system, err := promptbuilder.NewPrompt("shared rubric")
			if err != nil {
				t.Fatalf("NewPrompt (system): %v", err)
			}
			opts := append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
				claudeexecutor.WithSystemInstructions[errCapRequest, errCapResponse](system),
			}, tc.opts...)
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			resp, err := exec.Execute(t.Context(), errCapRequest{}, tc.tools)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got, want := resp.Answer, "42"; got != want {
				t.Errorf("resp.Answer: got = %q, want = %q", got, want)
			}

			reqs := requests()
			if got, want := len(reqs), 1; got != want {
				t.Fatalf("HTTP requests: got = %d, want = %d", got, want)
			}
			gotSystem, gotMessages := wireCacheMarkers(t, reqs[0])
			if gotSystem != tc.wantSystem {
				t.Errorf("system cache_control markers: got = %d, want = %d", gotSystem, tc.wantSystem)
			}
			if gotMessages != tc.wantMessageCCs {
				t.Errorf("message cache_control markers: got = %d, want = %d", gotMessages, tc.wantMessageCCs)
			}
		})
	}
}
