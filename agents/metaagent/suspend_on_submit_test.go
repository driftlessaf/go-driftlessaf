/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestClaudeAgentSuspendsOnSubmitAndResumesWithResults drives a routed Claude
// agent with SuspendOnSubmit through a scripted server: each submission
// suspends, and ResumeWithResults places the review verbatim as the submit
// call's result in the same conversation.
func TestClaudeAgentSuspendsOnSubmitAndResumesWithResults(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		writeToolUseTurn(t, w, n, "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`)
	}))
	t.Cleanup(srv.Close)

	selection := modelrouter.Selection{Provider: "scripted", LogicalModel: "claude-sonnet-4-6"}
	adapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{
		Provider: selection.Provider,
		Adapter: func(_ context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("scripted"), option.WithMaxRetries(0))
			return NewAnthropicMessagesBinding(plan, client.Messages, nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, routedTestRoute(selection, modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6")),
		AdapterRegistries{AnthropicMessages: adapters})
	cfg := routedTestConfig(t)
	cfg.SuspendOnSubmit = true
	agent, err := NewRouted[*testRequest](t.Context(), router, selection, cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx := agenttrace.WithTracer[*basetenTestResponse](t.Context(), new(basetenRecordingTracer))
	resp, err := agent.Execute(ctx, &testRequest{}, toolcall.EmptyTools{})
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.Reason != checkpoint.ReasonSubmitted {
		t.Fatalf("Execute: got %v, want a %q suspension", err, checkpoint.ReasonSubmitted)
	}
	if resp == nil || resp.Answer != "done" {
		t.Errorf("Execute's Resp: got %+v, want the accepted submission", resp)
	}
	if len(susp.PendingToolCalls) != 1 || susp.PendingToolCalls[0].ID != "tool-1" {
		t.Fatalf("PendingToolCalls: got %+v, want the submit call tool-1", susp.PendingToolCalls)
	}

	resumer, ok := AsResultResumer(agent)
	if !ok {
		t.Fatal("AsResultResumer() = false for a Claude-backed agent")
	}
	const review = "review: name the gap"
	_, err = resumer.ResumeWithResults(ctx, susp.Envelope, map[string]string{"tool-1": review}, toolcall.EmptyTools{})
	if again, ok := checkpoint.AsSuspension(err); !ok || again.PendingToolCalls[0].ID != "tool-2" {
		t.Fatalf("ResumeWithResults: got %v, want the second submission's suspension", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests: got %d, want 2", len(bodies))
	}
	var resumed struct {
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
	if err := json.Unmarshal(bodies[1], &resumed); err != nil {
		t.Fatal(err)
	}
	last := resumed.Messages[len(resumed.Messages)-1]
	var got []string
	for _, c := range last.Content {
		if c.Type == "tool_result" && c.ToolUseID == "tool-1" {
			for _, part := range c.Content {
				got = append(got, part.Text)
			}
		}
	}
	if len(got) != 1 || got[0] != review {
		t.Errorf("resumed submit result: got %q, want [%q] verbatim", got, review)
	}
}

// TestAsResultResumerReportsFalseForExecuteOnlyAgent: an Agent without
// ResumeWithResults reports false.
func TestAsResultResumerReportsFalseForExecuteOnlyAgent(t *testing.T) {
	var agent Agent[*testRequest, *testResponse, testCallbacks] = resumableAgent{}
	if _, ok := AsResultResumer(agent); ok {
		t.Error("AsResultResumer() = true for an agent without ResumeWithResults, want false")
	}
}
