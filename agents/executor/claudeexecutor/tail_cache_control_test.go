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
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// markerCounts is how many cache_control markers a request carries on each part.
type markerCounts struct {
	system, tools, messages int
}

func requestMarkers(t *testing.T, body []byte) markerCounts {
	t.Helper()
	type marked struct {
		CacheControl json.RawMessage `json:"cache_control"`
	}
	var req struct {
		System   []marked `json:"system"`
		Tools    []marked `json:"tools"`
		Messages []struct {
			Content []marked `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	var got markerCounts
	for _, s := range req.System {
		if len(s.CacheControl) > 0 {
			got.system++
		}
	}
	for _, tl := range req.Tools {
		if len(tl.CacheControl) > 0 {
			got.tools++
		}
	}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if len(c.CacheControl) > 0 {
				got.messages++
			}
		}
	}
	return got
}

func cacheLookupTools() map[string]claudetool.Metadata[errCapResponse] {
	return map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup"},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": true}
			},
		},
	}
}

func newCacheTestExecutor(t *testing.T, client anthropic.Client, opts ...claudeexecutor.Option[errCapRequest, errCapResponse]) claudeexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("large judged payload")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	system, err := promptbuilder.NewPrompt("shared rubric")
	if err != nil {
		t.Fatalf("NewPrompt (system): %v", err)
	}
	opts = append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithSystemInstructions[errCapRequest, errCapResponse](system),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
	}, opts...)
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

// TestWithoutTailCacheControlTwoTurns drives a successful two-turn run (a
// tool call, then submit) and checks both outgoing requests: the static
// markers on tools and system stay in every case, the tail marker rides the
// messages by default and is absent with WithoutTailCacheControl, and
// WithoutCacheControl removes every marker. WithCacheFirstUserBlock keeps its
// marker on the first user block independently of the tail option.
func TestWithoutTailCacheControlTwoTurns(t *testing.T) {
	type opt = claudeexecutor.Option[errCapRequest, errCapResponse]
	tailOff := claudeexecutor.WithoutTailCacheControl[errCapRequest, errCapResponse]()
	allOff := claudeexecutor.WithoutCacheControl[errCapRequest, errCapResponse]()
	firstUser := claudeexecutor.WithCacheFirstUserBlock[errCapRequest, errCapResponse]()

	for _, tc := range []struct {
		name string
		opts []opt
		// want holds the expected markers on request 1 and request 2.
		want [2]markerCounts
		// tailMarkersGrow expects the second request to carry more message
		// markers than the first (the tail advanced onto the tool result).
		tailMarkersGrow bool
	}{{
		name:            "default places static and tail markers",
		want:            [2]markerCounts{{system: 1, tools: 1, messages: 1}, {system: 1, tools: 1, messages: 2}},
		tailMarkersGrow: true,
	}, {
		name: "tail caching off keeps static markers only",
		opts: []opt{tailOff},
		want: [2]markerCounts{{system: 1, tools: 1}, {system: 1, tools: 1}},
	}, {
		name: "cache control off places no markers",
		opts: []opt{allOff},
		want: [2]markerCounts{},
	}, {
		name: "cache control off with tail option places no markers",
		opts: []opt{allOff, tailOff},
		want: [2]markerCounts{},
	}, {
		name: "first user block stays with tail caching off",
		opts: []opt{tailOff, firstUser},
		want: [2]markerCounts{{system: 1, tools: 1, messages: 1}, {system: 1, tools: 1, messages: 1}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := newExecTestClient(t, func(n int) []string {
				if n == 1 {
					return namedToolCallTurn(t, "msg_1", "toolu_lookup", "lookup", map[string]any{"reasoning": "look"})
				}
				return submitCallTurn(t, "msg_2", "toolu_submit", submitInput("42"))
			})
			exec := newCacheTestExecutor(t, client, tc.opts...)

			resp, err := exec.Execute(t.Context(), errCapRequest{}, cacheLookupTools())
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got, want := resp.Answer, "42"; got != want {
				t.Errorf("resp.Answer: got = %q, want = %q", got, want)
			}
			reqs := requests()
			if got, want := len(reqs), 2; got != want {
				t.Fatalf("HTTP requests: got = %d, want = %d", got, want)
			}
			for i, body := range reqs {
				got := requestMarkers(t, body)
				if tc.tailMarkersGrow && i == 1 {
					// Two tail markers are retained, so the second turn holds
					// the first-turn tail and the new one.
					if got.messages < 2 {
						t.Errorf("request %d message markers: got = %d, want >= 2", i+1, got.messages)
					}
					got.messages = tc.want[i].messages
				}
				if got != tc.want[i] {
					t.Errorf("request %d markers: got = %+v, want = %+v", i+1, got, tc.want[i])
				}
			}
		})
	}
}

// TestWithoutTailCacheControlResume pins that a resumed execution honours the
// option: the resumed request keeps the static markers and carries no tail
// marker, where the default resumed request places one on the rebuilt tail.
func TestWithoutTailCacheControlResume(t *testing.T) {
	type opt = claudeexecutor.Option[errCapRequest, errCapResponse]
	for _, tc := range []struct {
		name         string
		opts         []opt
		wantMessages int
	}{
		{name: "default places a tail marker", wantMessages: 1},
		{name: "tail caching off", opts: []opt{claudeexecutor.WithoutTailCacheControl[errCapRequest, errCapResponse]()}, wantMessages: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := newExecTestClient(t, func(n int) []string {
				if n == 1 {
					return wrapTurn("msg_1", toolUseBlockSSE(t, 0, "toolu_ask", askAFriendToolName, map[string]any{"question": "go on?"})...)
				}
				return submitCallTurn(t, "msg_2", "toolu_submit", submitInput("42"))
			})
			opts := append([]opt{claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](askAFriendProvider())}, tc.opts...)
			exec := newCacheTestExecutor(t, client, opts...)
			tools := cacheLookupTools()

			_, err := exec.Execute(t.Context(), errCapRequest{}, tools)
			susp, ok := checkpoint.AsSuspension(err)
			if !ok {
				t.Fatalf("Execute: got %v, want a suspension", err)
			}
			// The envelope crosses a store as JSON, as every caller keeps it.
			raw, err := json.Marshal(susp.Envelope)
			if err != nil {
				t.Fatal(err)
			}
			var env checkpoint.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			resumer, ok := exec.(claudeexecutor.ResultResumer[errCapRequest, errCapResponse])
			if !ok {
				t.Fatal("executor does not satisfy ResultResumer")
			}
			if _, err := resumer.ResumeWithResults(t.Context(), env, map[string]string{"toolu_ask": "yes"}, tools); err != nil {
				t.Fatalf("ResumeWithResults: %v", err)
			}
			reqs := requests()
			if got, want := len(reqs), 2; got != want {
				t.Fatalf("HTTP requests: got = %d, want = %d", got, want)
			}
			got := requestMarkers(t, reqs[1])
			if got.system != 1 || got.tools < 1 {
				t.Errorf("resumed static markers: got = system %d, tools %d, want = system 1, tools >= 1", got.system, got.tools)
			}
			if got.messages != tc.wantMessages {
				t.Errorf("resumed message markers: got = %d, want = %d", got.messages, tc.wantMessages)
			}
		})
	}
}
