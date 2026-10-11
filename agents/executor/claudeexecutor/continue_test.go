/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// continueServer is one validating server shared by an exhausted run and the
// executor that continues it, so both read one request count. Every turn
// calls "lookup", a tool no run registers, except request submitAt, which
// submits. Zero never submits.
type continueServer struct {
	client anthropic.Client

	mu     sync.Mutex
	bodies [][]byte
}

func newContinueServer(t *testing.T, submitAt int) *continueServer {
	t.Helper()
	s := &continueServer{}
	srv := newValidatingAnthropicServer(t, func(reqNum int, body []byte) []string {
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if reqNum == submitAt {
			return wrapTurn(fmt.Sprintf("msg_submit_%d", reqNum),
				toolUseBlockSSE(t, 0, fmt.Sprintf("toolu_submit_%d", reqNum), "submit_result", submitInput("continued"))...)
		}
		return wrapTurn(fmt.Sprintf("msg_loop_%d", reqNum),
			toolUseBlockSSE(t, 0, fmt.Sprintf("toolu_loop_%d", reqNum), "lookup",
				map[string]any{"reasoning": "keep digging"})...)
	})
	s.client = anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	return s
}

// executor builds an executor over the server with the submit tool and
// maxTurns, plus opts. An exhausted run and the executor continuing it differ
// only in maxTurns unless a test drifts them on purpose.
func (s *continueServer) executor(t *testing.T, maxTurns int, opts ...claudeexecutor.Option[errCapRequest, errCapResponse]) claudeexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](s.client, prompt, append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](maxTurns),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
	}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

// requests reports how many model requests the server has seen.
func (s *continueServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// messages returns the messages of the server's nth request, 1-based.
func (s *continueServer) messages(t *testing.T, n int) []json.RawMessage {
	t.Helper()
	s.mu.Lock()
	body := s.bodies[n-1]
	s.mu.Unlock()
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request %d: %v", n, err)
	}
	return req.Messages
}

// exhaustedTurns is the budget every exhausted run below spends.
const exhaustedTurns = 2

// exhaust runs a fresh Execute at exhaustedTurns to the end of its budget and
// returns the conversation the exhaustion carries.
func (s *continueServer) exhaust(t *testing.T) checkpoint.Envelope {
	t.Helper()
	_, err := s.executor(t, exhaustedTurns).Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{})
	return *exhaustedContinuation(t, err, exhaustedTurns)
}

func continuer(t *testing.T, exec claudeexecutor.Interface[errCapRequest, errCapResponse]) claudeexecutor.Continuer[errCapRequest, errCapResponse] {
	t.Helper()
	c, ok := exec.(claudeexecutor.Continuer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor does not satisfy Continuer")
	}
	return c
}

// TestContinueRunsTheRestOfTheBudget pins the budget semantics: the
// continuing executor's MaxTurns counts the whole conversation, so after a
// 2-turn run a 5-turn executor runs exactly 3 more, its first request carries
// the exhausted transcript unchanged, and its own exhaustion is again
// continuable, from the conversation's last turn.
func TestContinueRunsTheRestOfTheBudget(t *testing.T) {
	const base, raised = exhaustedTurns, 5
	srv := newContinueServer(t, 0)
	env := srv.exhaust(t)
	// The exhausted transcript is the last request's messages plus that
	// turn's assistant message and the user message answering it.
	exhaustedMessages := len(srv.messages(t, base)) + 2

	_, err := continuer(t, srv.executor(t, raised)).Continue(t.Context(), env, map[string]claudetool.Metadata[errCapResponse]{})
	again := exhaustedContinuation(t, err, raised-base)

	if got, want := srv.requests(), raised; got != want {
		t.Errorf("provider requests: got %d, want %d (%d before the continuation, %d after)", got, want, base, raised-base)
	}
	if got := len(srv.messages(t, base+1)); got != exhaustedMessages {
		t.Errorf("first continued request: %d messages, want %d (the exhausted transcript, sent as it was)", got, exhaustedMessages)
	}
	if got, want := again.Turn, raised-1; got != want {
		t.Errorf("Continuation.Turn: got %d, want %d (the conversation's last turn)", got, want)
	}
}

// TestContinueReturnsTheSubmittedResult pins that a continued conversation
// ends the way any run does: a submit on a continued turn is the result.
func TestContinueReturnsTheSubmittedResult(t *testing.T) {
	const base = exhaustedTurns
	srv := newContinueServer(t, base+2)
	env := srv.exhaust(t)

	resp, err := continuer(t, srv.executor(t, 10)).Continue(t.Context(), env, map[string]claudetool.Metadata[errCapResponse]{})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if resp.Answer != "continued" {
		t.Errorf("Answer: got %q, want %q", resp.Answer, "continued")
	}
	if got, want := srv.requests(), base+2; got != want {
		t.Errorf("provider requests: got %d, want %d", got, want)
	}
}

// TestContinueRefuses pins every refusal Continue makes before it sends a
// request: configuration drift (as checkpoint.ErrConfigDrift, so a caller
// can fall back to a fresh run), a budget the conversation has already
// spent, and a suspension, which waits on answers only Resume pairs. And
// the converse: Resume refuses an exhausted conversation.
func TestContinueRefuses(t *testing.T) {
	const base = exhaustedTurns
	lookup := map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {Definition: anthropic.ToolParam{
			Name:        "lookup",
			InputSchema: anthropic.ToolInputSchemaParam{Type: "object"},
		}},
	}
	tests := []struct {
		name      string
		maxTurns  int
		opts      []claudeexecutor.Option[errCapRequest, errCapResponse]
		tools     map[string]claudetool.Metadata[errCapResponse]
		mutate    func(*checkpoint.Envelope)
		wantDrift bool
	}{{
		name:      "a different tool set is drift",
		maxTurns:  10,
		tools:     lookup,
		wantDrift: true,
	}, {
		name:      "a different model is drift",
		maxTurns:  10,
		opts:      []claudeexecutor.Option[errCapRequest, errCapResponse]{claudeexecutor.WithModel[errCapRequest, errCapResponse]("claude-opus-4-6")},
		wantDrift: true,
	}, {
		name:     "a budget no larger than the turns already run",
		maxTurns: base,
	}, {
		// A negative turn would make the budget exceed maxTurns.
		name:     "a negative turn",
		maxTurns: 10,
		mutate:   func(env *checkpoint.Envelope) { env.Turn = -5 },
	}, {
		name:     "a suspension",
		maxTurns: 10,
		mutate: func(env *checkpoint.Envelope) {
			env.PendingToolCalls = []checkpoint.PendingToolCall{{ID: "toolu_ask", Name: askAFriendToolName}}
			env.RemainingTurns = 5
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newContinueServer(t, 0)
			env := srv.exhaust(t)
			if tt.mutate != nil {
				tt.mutate(&env)
			}
			tools := tt.tools
			if tools == nil {
				tools = map[string]claudetool.Metadata[errCapResponse]{}
			}

			_, err := continuer(t, srv.executor(t, tt.maxTurns, tt.opts...)).Continue(t.Context(), env, tools)
			if err == nil {
				t.Fatal("Continue: got nil error, want a refusal")
			}
			if got := errors.Is(err, checkpoint.ErrConfigDrift); got != tt.wantDrift {
				t.Errorf("errors.Is(ErrConfigDrift) = %v, want %v (err %v)", got, tt.wantDrift, err)
			}
			if got := srv.requests(); got != base {
				t.Errorf("provider requests: got %d, want %d (a refusal sends nothing)", got, base)
			}
		})
	}

	t.Run("resume refuses an exhausted conversation", func(t *testing.T) {
		srv := newContinueServer(t, 0)
		env := srv.exhaust(t)
		resumer, ok := srv.executor(t, 10).(claudeexecutor.Resumer[errCapRequest, errCapResponse])
		if !ok {
			t.Fatal("executor does not satisfy Resumer")
		}
		if _, err := resumer.Resume(t.Context(), env, nil, map[string]claudetool.Metadata[errCapResponse]{}); err == nil {
			t.Fatal("Resume: got nil error, want a refusal (nothing is pending and no budget remains)")
		}
		if got := srv.requests(); got != base {
			t.Errorf("provider requests: got %d, want %d", got, base)
		}
	})
}
