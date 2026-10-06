/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	stdcmp "cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
)

const (
	hitCount    = "count"
	hitMessages = "messages"
)

// countTokensFields is the request field set count_tokens accepts on both
// Vertex and Anthropic direct. The double rejects any other field with the 400 the real endpoint returns, so
// a count request that would fail in production fails here too.
var countTokensFields = map[string]struct{}{
	"model": {}, "messages": {}, "system": {}, "tools": {}, "tool_choice": {}, "thinking": {},
}

// budgetSpy records every request the budget server receives, in order.
type budgetSpy struct {
	mu            sync.Mutex
	order         []string
	countBodies   [][]byte
	counts        []int64
	messageBodies [][]byte
}

func (s *budgetSpy) snapshot() (order []string, countBodies [][]byte, counts []int64, messageBodies [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...), append([][]byte(nil), s.countBodies...),
		append([]int64(nil), s.counts...), append([][]byte(nil), s.messageBodies...)
}

// budgetServer configures the double for /v1/messages/count_tokens and
// /v1/messages.
type budgetServer struct {
	// bytesPerToken sets the count: the request body length divided by
	// bytesPerToken, rounded up. The count is a function of the body alone,
	// so the same request always counts the same, a reduced request counts
	// less, and no count exceeds the body's bytes, which is the property the
	// executor's local upper bound relies on.
	bytesPerToken int
	// countStatus returns the HTTP status for the nth count call; 0 or 200
	// answers with the count.
	countStatus func(n int) int
	// turn returns the SSE events for the nth /v1/messages call.
	turn func(n int) []string
}

func newBudgetServer(t *testing.T, cfg budgetServer) (anthropic.Client, *budgetSpy) {
	t.Helper()
	spy := &budgetSpy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			spy.mu.Lock()
			spy.order = append(spy.order, hitCount)
			spy.countBodies = append(spy.countBodies, body)
			n := len(spy.countBodies)
			spy.mu.Unlock()

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Errorf("count request %d: invalid JSON: %v", n, err)
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error")
				return
			}
			for k := range fields {
				if _, ok := countTokensFields[k]; !ok {
					t.Errorf("count request %d: field %q is not accepted by count_tokens", n, k)
					writeAPIError(w, http.StatusBadRequest, "invalid_request_error")
					return
				}
			}
			if status := cfg.countStatus; status != nil {
				if code := status(n); code != 0 && code != http.StatusOK {
					errType := "invalid_request_error"
					if code == 529 {
						errType = "overloaded_error"
					}
					writeAPIError(w, code, errType)
					return
				}
			}
			tokens := int64((len(body) + cfg.bytesPerToken - 1) / cfg.bytesPerToken)
			spy.mu.Lock()
			spy.counts = append(spy.counts, tokens)
			spy.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"input_tokens":%d}`, tokens)
		case "/v1/messages":
			spy.mu.Lock()
			spy.order = append(spy.order, hitMessages)
			spy.messageBodies = append(spy.messageBodies, body)
			n := len(spy.messageBodies)
			spy.mu.Unlock()

			assertRequestValid(t, n, body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseBody(t, cfg.turn(n)))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	), spy
}

func writeAPIError(w http.ResponseWriter, code int, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":"scripted"}}`, errType)
}

// boundPrompt returns a prompt whose text is s, which NewPrompt cannot take
// directly because it accepts only string literals.
func boundPrompt(t *testing.T, s string) *promptbuilder.Prompt {
	t.Helper()
	p, err := promptbuilder.NewPrompt("{{data}}")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	if p, err = p.BindJSON("data", s); err != nil {
		t.Fatalf("BindJSON: %v", err)
	}
	return p
}

// pageTool returns a tool whose result is a page of the size its "bytes"
// input names, with a top-level next_offset paging pointer.
func pageTool(description string) claudetool.Metadata[errCapResponse] {
	return claudetool.Metadata[errCapResponse]{
		Definition: anthropic.ToolParam{
			Name:        "list_commits",
			Description: anthropic.String(description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{
				"bytes": map[string]any{"type": "integer"},
			}},
		},
		Handler: func(_ context.Context, toolUse anthropic.ToolUseBlock, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) map[string]any {
			var in struct {
				Bytes int `json:"bytes"`
			}
			if err := json.Unmarshal(toolUse.Input, &in); err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"next_offset": 20, "truncated": true, "commits": strings.Repeat("c", in.Bytes)}
		},
	}
}

func pageTools(description string) map[string]claudetool.Metadata[errCapResponse] {
	return map[string]claudetool.Metadata[errCapResponse]{"list_commits": pageTool(description)}
}

func pageSeed(id string, n int) anthropic.ToolUseBlock {
	return anthropic.ToolUseBlock{ID: id, Name: "list_commits", Input: json.RawMessage(fmt.Sprintf(`{"bytes":%d}`, n))}
}

// budgetRun is one executor run against the budget server.
type budgetRun struct {
	prompt string
	opts   []claudeexecutor.Option[errCapRequest, errCapResponse]
	tools  map[string]claudetool.Metadata[errCapResponse]
	seeds  []anthropic.ToolUseBlock
}

func (r budgetRun) execute(t *testing.T, client anthropic.Client, extra ...claudeexecutor.Option[errCapRequest, errCapResponse]) (errCapResponse, error) {
	t.Helper()
	prompt := boundPrompt(t, stdcmp.Or(r.prompt, "Answer the question."))
	opts := append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(3)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	}, r.opts...)
	exec, err := claudeexecutor.New(client, prompt, append(opts, extra...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec.Execute(t.Context(), errCapRequest{}, r.tools, r.seeds...)
}

// windowFor returns a context window whose input budget at maxTokens is
// exactly budget, by the inputBudget formula: window - maxTokens - window/100.
func windowFor(t *testing.T, budget, maxTokens int64) int64 {
	t.Helper()
	for w := budget + maxTokens; w <= 2*(budget+maxTokens); w++ {
		if w-maxTokens-w/100 == budget {
			return w
		}
	}
	t.Fatalf("no window gives budget %d at max_tokens %d", budget, maxTokens)
	return 0
}

// probeCountBytes returns the byte length of the first count request run
// sends, which is the same request on every run because the body depends
// only on the conversation, not on the window.
func probeCountBytes(t *testing.T, run budgetRun) int {
	t.Helper()
	client, spy := newBudgetServer(t, budgetServer{bytesPerToken: 1, turn: func(int) []string { return answerTurn }})
	// A 20,000-token window forces a count for any request over about 8 KB.
	_, _ = run.execute(t, client, claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](20000))
	_, bodies, _, _ := spy.snapshot()
	if len(bodies) == 0 {
		t.Fatal("probe: the run made no count call")
	}
	return len(bodies[0])
}

func ceilDiv(n, d int) int64 { return int64((n + d - 1) / d) }

// TestInputBudgetLimits proves that each serving model gets its own window,
// the output headroom is the max_tokens sent, an override wins over the
// registry, and New fails when no window is known or the headroom leaves no
// input budget.
func TestInputBudgetLimits(t *testing.T) {
	t.Parallel()

	type opts = []claudeexecutor.Option[errCapRequest, errCapResponse]
	tests := []struct {
		name       string
		opts       opts
		wantNewErr string
		want       claudeexecutor.InputTooLargeError
	}{{
		name: "opus 5.5 from the registry at the metaagent default",
		opts: opts{
			claudeexecutor.WithModel[errCapRequest, errCapResponse]("claude-opus-5-5"),
			claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](32000),
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](0),
		},
		want: claudeexecutor.InputTooLargeError{Model: "claude-opus-5-5", ContextWindow: 1_000_000, OutputHeadroom: 32000, Budget: 958000},
	}, {
		name: "opus 4.8 from the registry at the executor default",
		opts: opts{
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](0),
			claudeexecutor.WithModel[errCapRequest, errCapResponse]("claude-opus-4-8@default"),
		},
		want: claudeexecutor.InputTooLargeError{Model: "claude-opus-4-8@default", ContextWindow: 1_000_000, OutputHeadroom: 8192, Budget: 981808},
	}, {
		name: "opus 5.5 at the output ceiling",
		opts: opts{
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](0),
			claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](128000),
			claudeexecutor.WithModel[errCapRequest, errCapResponse]("claude-opus-5-5"),
		},
		want: claudeexecutor.InputTooLargeError{Model: "claude-opus-5-5", ContextWindow: 1_000_000, OutputHeadroom: 128000, Budget: 862000},
	}, {
		name: "routed opus 5.5 reports the provider model id",
		opts: opts{
			claudeexecutor.WithRoutedModel[errCapRequest, errCapResponse]("claude-opus-5-5@20261001", "claude-opus-5-5"),
			claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](32000),
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](0),
		},
		want: claudeexecutor.InputTooLargeError{Model: "claude-opus-5-5@20261001", ContextWindow: 1_000_000, OutputHeadroom: 32000, Budget: 958000},
	}, {
		name: "an override for a model with no registry window",
		opts: opts{
			claudeexecutor.WithModel[errCapRequest, errCapResponse]("claude-sonnet-4-6"),
			claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](32000),
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](200000),
		},
		want: claudeexecutor.InputTooLargeError{Model: "claude-sonnet-4-6", ContextWindow: 200000, OutputHeadroom: 32000, Budget: 166000},
	}, {
		name: "an override wins over the registry",
		opts: opts{
			claudeexecutor.WithModel[errCapRequest, errCapResponse]("claude-opus-5-5"),
			claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](32000),
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](300000),
		},
		want: claudeexecutor.InputTooLargeError{Model: "claude-opus-5-5", ContextWindow: 300000, OutputHeadroom: 32000, Budget: 265000},
	}, {
		name:       "a model with no registry window and no override",
		opts:       opts{claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](0)},
		wantNewErr: "no context window is known",
	}, {
		name: "an unknown Claude model with no override",
		opts: opts{
			claudeexecutor.WithModel[errCapRequest, errCapResponse]("claude-made-up-9"),
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](0),
		},
		wantNewErr: "no context window is known",
	}, {
		name: "headroom that fills the window",
		opts: opts{
			claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](128000),
			claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](129000),
		},
		wantNewErr: "leaves no input budget",
	}, {
		name:       "a negative window",
		opts:       opts{claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](-1)},
		wantNewErr: "must not be negative",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// One token per byte is the highest count an honest tokenizer
			// gives, so a system prompt one byte over the budget cannot fit.
			client, spy := newBudgetServer(t, budgetServer{bytesPerToken: 1, turn: func(int) []string { return answerTurn }})
			prompt, err := promptbuilder.NewPrompt("Answer the question.")
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			opts := append(opts{
				claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(3)),
				claudeexecutor.WithSystemInstructions[errCapRequest, errCapResponse](boundPrompt(t, strings.Repeat("s", int(tt.want.Budget)+1))),
			}, tt.opts...)
			exec, err := claudeexecutor.New(client, prompt, opts...)
			if tt.wantNewErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantNewErr) {
					t.Fatalf("New: got = %v, want an error containing %q", err, tt.wantNewErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, execErr := exec.Execute(t.Context(), errCapRequest{}, nil)
			tooLarge, ok := errors.AsType[*claudeexecutor.InputTooLargeError](execErr)
			if !ok {
				t.Fatalf("Execute: got = %v, want an *InputTooLargeError", execErr)
			}
			order, _, counts, _ := spy.snapshot()
			if diff := cmp.Diff([]string{hitCount}, order); diff != "" {
				t.Errorf("request order (-want, +got):\n%s", diff)
			}
			want := tt.want
			want.InputTokens = counts[0]
			want.ReducedInputTokens = counts[0]
			if diff := cmp.Diff(&want, tooLarge); diff != "" {
				t.Errorf("InputTooLargeError (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestInputBudgetCountsEveryComponent proves that each request component alone
// pushes the local bound over the budget and reaches the count request, which
// carries system, tools and messages with their cache_control markers.
func TestInputBudgetCountsEveryComponent(t *testing.T) {
	t.Parallel()

	const (
		componentBytes = 40000
		// 40,000 - 8,192 - 400 = 31,408 tokens of budget: a 40,000-byte
		// component exceeds the local bound and fits the 2-byte-per-token
		// count.
		window = 40000
	)
	pad := func(tag string) string { return tag + strings.Repeat("z", componentBytes) }
	toolTurn := namedToolCallTurn(t, "msg_01", "toolu_old", "list_commits", map[string]any{"bytes": componentBytes})
	tests := []struct {
		name      string
		run       budgetRun
		turn      func(n int) []string
		tag       string
		field     string
		wantOrder []string
	}{{
		name:      "a request under the local bound makes no count call",
		run:       budgetRun{tools: pageTools("List commits.")},
		turn:      func(int) []string { return answerTurn },
		wantOrder: []string{hitMessages},
	}, {
		name: "a large system prompt",
		run: budgetRun{
			tools: pageTools("List commits."),
			opts: []claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithSystemInstructions[errCapRequest, errCapResponse](boundPrompt(t, pad("SYSTEM-TAG"))),
			},
		},
		turn:      func(int) []string { return answerTurn },
		tag:       "SYSTEM-TAG",
		field:     "system",
		wantOrder: []string{hitCount, hitMessages},
	}, {
		name:      "a large tool definition",
		run:       budgetRun{tools: pageTools(pad("TOOL-TAG"))},
		turn:      func(int) []string { return answerTurn },
		tag:       "TOOL-TAG",
		field:     "tools",
		wantOrder: []string{hitCount, hitMessages},
	}, {
		name: "a large first user block cached with WithCacheFirstUserBlock",
		run: budgetRun{
			prompt: pad("USER-TAG"),
			tools:  pageTools("List commits."),
			opts: []claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithCacheFirstUserBlock[errCapRequest, errCapResponse](),
			},
		},
		turn:      func(int) []string { return answerTurn },
		tag:       "USER-TAG",
		field:     "messages",
		wantOrder: []string{hitCount, hitMessages},
	}, {
		name: "an old tool result from an earlier turn",
		run:  budgetRun{tools: pageTools("List commits.")},
		turn: func(n int) []string {
			if n == 1 {
				return toolTurn
			}
			return answerTurn
		},
		tag:       `"toolu_old"`,
		field:     "messages",
		wantOrder: []string{hitMessages, hitCount, hitMessages},
	}, {
		name: "seed tool calls",
		run: budgetRun{
			tools: pageTools("List commits."),
			seeds: []anthropic.ToolUseBlock{pageSeed("toolu_seed", componentBytes)},
		},
		turn:      func(int) []string { return answerTurn },
		tag:       `"toolu_seed"`,
		field:     "messages",
		wantOrder: []string{hitCount, hitMessages},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, spy := newBudgetServer(t, budgetServer{bytesPerToken: 2, turn: tt.turn})
			// Every row has a small system prompt so the system marker is
			// present; a row's own system prompt comes later and wins.
			run := tt.run
			run.opts = append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
				claudeexecutor.WithSystemInstructions[errCapRequest, errCapResponse](boundPrompt(t, "You list commits.")),
			}, run.opts...)
			if _, err := run.execute(t, client, claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](window)); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			order, countBodies, _, _ := spy.snapshot()
			if diff := cmp.Diff(tt.wantOrder, order); diff != "" {
				t.Fatalf("request order (-want, +got):\n%s", diff)
			}
			for i, body := range countBodies {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatalf("count request %d: %v", i+1, err)
				}
				for _, k := range []string{"system", "tools", "messages"} {
					if !strings.Contains(string(fields[k]), `"cache_control"`) {
						t.Errorf("count request %d: %s carries no cache_control marker", i+1, k)
					}
				}
				if !strings.Contains(string(fields[tt.field]), tt.tag) {
					t.Errorf("count request %d: %s does not carry the component tagged %s", i+1, tt.field, tt.tag)
				}
			}
			if tt.field == "messages" && tt.tag == "USER-TAG" {
				assertFirstUserBlockCached(t, countBodies[0])
			}
		})
	}
}

func assertFirstUserBlockCached(t *testing.T, body []byte) {
	t.Helper()
	var req struct {
		Messages []struct {
			Content []struct {
				Text         string           `json:"text"`
				CacheControl *json.RawMessage `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("count request: %v", err)
	}
	if len(req.Messages) == 0 || len(req.Messages[0].Content) == 0 {
		t.Fatal("count request: no first user block")
	}
	first := req.Messages[0].Content[0]
	if !strings.Contains(first.Text, "USER-TAG") || first.CacheControl == nil {
		t.Errorf("count request: first user block cached = %t, carries the prompt = %t; want both", first.CacheControl != nil, strings.Contains(first.Text, "USER-TAG"))
	}
}

// budgetOrderRun has two older results, 40,000 and 8,000 bytes, and a small
// latest result. Only the two older results are eligible for reduction.
func budgetOrderRun() budgetRun {
	return budgetRun{
		tools: pageTools("List commits."),
		seeds: []anthropic.ToolUseBlock{
			pageSeed("toolu_p1", 40000),
			pageSeed("toolu_p2", 8000),
			pageSeed("toolu_p3", 100),
		},
	}
}

// TestInputBudgetOrder proves that a request at the budget calls the provider,
// a request over it is reduced and recounted, and a request that cannot fit is
// rejected with a typed error after at most three counts and no provider call.
func TestInputBudgetOrder(t *testing.T) {
	t.Parallel()

	countBytes := probeCountBytes(t, budgetOrderRun())
	tests := []struct {
		name          string
		bytesPerToken int
		// over is how many tokens the first count exceeds the budget by.
		over        int64
		wantOrder   []string
		wantReduced []string
		wantReject  bool
	}{{
		name:          "a count equal to the budget calls the provider",
		bytesPerToken: 2,
		over:          0,
		wantOrder:     []string{hitCount, hitMessages},
	}, {
		name:          "one token over reduces the oldest result, recounts and calls the provider",
		bytesPerToken: 2,
		over:          1,
		wantOrder:     []string{hitCount, hitCount, hitMessages},
		wantReduced:   []string{"toolu_p1"},
	}, {
		// At 8 bytes per token one reduction saves half of what the 4-byte
		// estimate expects, so the first recount is still over, every
		// remaining result is reduced, and the last recount is still over.
		name:          "a request that cannot fit is rejected after reduce, recount, reduce, recount",
		bytesPerToken: 8,
		over:          6000,
		wantOrder:     []string{hitCount, hitCount, hitCount},
		wantReject:    true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first := ceilDiv(countBytes, tt.bytesPerToken)
			budget := first - tt.over
			window := windowFor(t, budget, 8192)
			client, spy := newBudgetServer(t, budgetServer{bytesPerToken: tt.bytesPerToken, turn: func(int) []string { return answerTurn }})

			got, err := budgetOrderRun().execute(t, client, claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](window))
			order, countBodies, counts, messageBodies := spy.snapshot()
			if diff := cmp.Diff(tt.wantOrder, order); diff != "" {
				t.Errorf("request order (-want, +got):\n%s", diff)
			}
			if len(countBodies[0]) != countBytes {
				t.Fatalf("first count request: got = %d bytes, want = %d (the probe's request)", len(countBodies[0]), countBytes)
			}
			for i := 1; i < len(countBodies); i++ {
				if len(countBodies[i]) >= len(countBodies[i-1]) {
					t.Errorf("count request %d: got = %d bytes, want fewer than the %d before it", i+1, len(countBodies[i]), len(countBodies[i-1]))
				}
			}

			if !tt.wantReject {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if got.Answer != "42" {
					t.Errorf("Answer: got = %q, want = %q", got.Answer, "42")
				}
				for _, id := range []string{"toolu_p1", "toolu_p2", "toolu_p3"} {
					wantReduced := slices.Contains(tt.wantReduced, id)
					if gotReduced := resultReduced(t, messageBodies[0], id); gotReduced != wantReduced {
						t.Errorf("%s reduced in the sent request: got = %t, want = %t", id, gotReduced, wantReduced)
					}
				}
				return
			}

			// The rejection is typed, carries every field, and the provider was
			// never called.
			if len(messageBodies) != 0 {
				t.Errorf("/v1/messages requests: got = %d, want = 0", len(messageBodies))
			}
			if !errors.Is(err, agentexecutor.ErrInputTooLarge) {
				t.Errorf("errors.Is(err, ErrInputTooLarge): got = false for %v", err)
			}
			tooLarge, ok := errors.AsType[*claudeexecutor.InputTooLargeError](err)
			if !ok {
				t.Fatalf("Execute: got = %v, want an *InputTooLargeError", err)
			}
			want := &claudeexecutor.InputTooLargeError{
				Model:              "claude-sonnet-4-6",
				ContextWindow:      window,
				OutputHeadroom:     8192,
				Budget:             budget,
				InputTokens:        first,
				ReducedInputTokens: counts[len(counts)-1],
				ReducedResults:     2,
			}
			if diff := cmp.Diff(want, tooLarge); diff != "" {
				t.Errorf("InputTooLargeError (-want, +got):\n%s", diff)
			}
			if _, isRequeue := workqueue.GetRequeueDelay(err); isRequeue {
				t.Error("rejection carries a requeue delay, want a terminal error")
			}
			for _, s := range []string{"failed to stream Claude response", "exceeded maximum conversation turns", "api_error", "rate_limit_error", "overloaded_error"} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error text %q contains %q, which another classifier matches", err.Error(), s)
				}
			}
		})
	}
}

// resultReduced reports whether the tool_result for id in a messages request
// body is a reduced result.
func resultReduced(t *testing.T, body []byte, id string) bool {
	t.Helper()
	var req struct {
		Messages []struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("messages request: %v", err)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == id {
				return len(b.Content) == 1 && strings.Contains(b.Content[0].Text, `"executor_reduced":true`)
			}
		}
	}
	t.Fatalf("messages request: no tool_result for %s", id)
	return false
}

// TestInputBudgetCountErrors proves that a transient count failure retries
// like a streamed turn, an exhausted one requeues, and a permanent one fails
// without calling the provider.
func TestInputBudgetCountErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		countStatus func(n int) int
		wantOrder   []string
		wantErr     string
		wantRequeue bool
	}{{
		name: "a 529 then a count retries and proceeds",
		countStatus: func(n int) int {
			if n == 1 {
				return 529
			}
			return http.StatusOK
		},
		wantOrder: []string{hitCount, hitCount, hitMessages},
	}, {
		name:        "a 529 on every attempt requeues after the retry budget",
		countStatus: func(int) int { return 529 },
		// fastRetry(3) is one attempt and three retries.
		wantOrder:   []string{hitCount, hitCount, hitCount, hitCount},
		wantErr:     "overloaded_error",
		wantRequeue: true,
	}, {
		name:        "a 400 fails the turn without a retry",
		countStatus: func(int) int { return http.StatusBadRequest },
		wantOrder:   []string{hitCount},
		wantErr:     "failed to count Claude request tokens",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, spy := newBudgetServer(t, budgetServer{bytesPerToken: 2, countStatus: tt.countStatus, turn: func(int) []string { return answerTurn }})
			run := budgetRun{tools: pageTools("List commits."), seeds: []anthropic.ToolUseBlock{pageSeed("toolu_seed", 40000)}}
			_, err := run.execute(t, client, claudeexecutor.WithInputBudget[errCapRequest, errCapResponse](40000))
			order, _, _, _ := spy.snapshot()
			if diff := cmp.Diff(tt.wantOrder, order); diff != "" {
				t.Errorf("request order (-want, +got):\n%s", diff)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Execute: got = %v, want an error containing %q", err, tt.wantErr)
			}
			if errors.Is(err, agentexecutor.ErrInputTooLarge) {
				t.Error("a count failure matches ErrInputTooLarge")
			}
			if strings.Contains(err.Error(), "failed to stream Claude response") {
				t.Errorf("a count failure reads as a stream failure: %v", err)
			}
			if _, gotRequeue := workqueue.GetRequeueDelay(err); gotRequeue != tt.wantRequeue {
				t.Errorf("requeue: got = %t, want = %t", gotRequeue, tt.wantRequeue)
			}
		})
	}
}

// TestInputBudgetUnsetMakesNoCountCalls proves that without the option, even a
// request far over any window goes straight to the provider.
func TestInputBudgetUnsetMakesNoCountCalls(t *testing.T) {
	t.Parallel()

	client, spy := newBudgetServer(t, budgetServer{bytesPerToken: 1, turn: func(int) []string { return answerTurn }})
	run := budgetRun{
		tools: pageTools("List commits."),
		seeds: []anthropic.ToolUseBlock{pageSeed("toolu_p1", 300000), pageSeed("toolu_p2", 300000)},
		opts: []claudeexecutor.Option[errCapRequest, errCapResponse]{
			claudeexecutor.WithSystemInstructions[errCapRequest, errCapResponse](boundPrompt(t, strings.Repeat("s", 300000))),
		},
	}
	if _, err := run.execute(t, client); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	order, _, _, messageBodies := spy.snapshot()
	if diff := cmp.Diff([]string{hitMessages}, order); diff != "" {
		t.Errorf("request order (-want, +got):\n%s", diff)
	}
	if resultReduced(t, messageBodies[0], "toolu_p1") {
		t.Error("toolu_p1 reduced without WithInputBudget")
	}
	if got := len(messageBodies[0]); got < 900000 {
		t.Errorf("messages request: got = %d bytes, want the whole 900,000-byte conversation", got)
	}
}

// TestInputTooLargeErrorUnwrap pins that callers match the rejection with
// the provider-neutral sentinel.
func TestInputTooLargeErrorUnwrap(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("turn 3: %w", &claudeexecutor.InputTooLargeError{Model: "claude-opus-5-5"})
	if !errors.Is(err, agentexecutor.ErrInputTooLarge) {
		t.Errorf("errors.Is(%v, ErrInputTooLarge): got = false, want = true", err)
	}
	if errors.Is(err, agentexecutor.ErrMaxTurns) {
		t.Errorf("errors.Is(%v, ErrMaxTurns): got = true, want = false", err)
	}
}
