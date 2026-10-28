/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

// suspendConfig returns a config with the ask-a-friend suspend tool enabled, over
// the standard test tool composition.
func suspendConfig(t *testing.T) Config[*testResponse, testCallbacks] {
	t.Helper()
	userPrompt, err := promptbuilder.NewPrompt("payload")
	if err != nil {
		t.Fatalf("NewPrompt() error = %v", err)
	}
	return Config[*testResponse, testCallbacks]{
		UserPrompt: userPrompt,
		Tools: toolcall.NewFindingToolsProvider[*testResponse, toolcall.WorktreeTools[toolcall.EmptyTools]](
			toolcall.NewWorktreeToolsProvider[*testResponse, toolcall.EmptyTools](
				toolcall.NewEmptyToolsProvider[*testResponse]())),
		SuspendToolName:        "ask_a_friend",
		SuspendToolDescription: "Ask the human operator a question and pause until they answer.",
	}
}

// TestSuspendToolRejectedOnGeminiBackend pins the fail-closed contract for the
// backend without executor suspend support: a set SuspendToolName must be a
// clear construction error, never silently dropped — a run whose model was
// promised an ask-a-friend tool that is never advertised could neither pause nor
// be diagnosed.
func TestSuspendToolRejectedOnGeminiBackend(t *testing.T) {
	_, err := New[*testRequest](t.Context(), "test-project", "us-central1", "gemini-2.5-flash", suspendConfig(t))
	if err == nil {
		t.Fatal("New() with SuspendToolName on Gemini: got nil, want error")
	}
	if !strings.Contains(err.Error(), "not yet supported on the Gemini backend") {
		t.Errorf("New() error = %v, want a clear not-yet-supported error", err)
	}
}

// TestSuspendToolRejectedOnOpenAIBackend is the OpenAI-compatible twin of the
// Gemini test above. The rejection fires before credential acquisition, so no
// fake ADC is needed.
func TestSuspendToolRejectedOnOpenAIBackend(t *testing.T) {
	_, err := New[*testRequest](t.Context(), "test-project", "us-central1", "google/gemini-2.5-pro", suspendConfig(t))
	if err == nil {
		t.Fatal("New() with SuspendToolName on OpenAI-compat: got nil, want error")
	}
	if !strings.Contains(err.Error(), "not yet supported on the OpenAI-compatible backend") {
		t.Errorf("New() error = %v, want a clear not-yet-supported error", err)
	}
}

// TestClaudeAgentIsResumer proves the Claude backend constructs with the
// suspend tool enabled and exposes the Resumer capability through AsResumer.
// Construction acquires Application Default Credentials (the Vertex path), so
// the test points GOOGLE_APPLICATION_CREDENTIALS at a hermetic fake key — no
// network is touched.
func TestClaudeAgentIsResumer(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))

	agent, err := New[*testRequest](t.Context(), "test-project", "us-central1", "claude-test-model", suspendConfig(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, ok := AsResumer[*testRequest](agent); !ok {
		t.Error("AsResumer() = false for a Claude-backed agent, want true")
	}
}

// TestAsResumerReportsFalseForExecuteOnlyAgent pins AsResumer's fallback
// contract: an Agent without the Resume method must report false so callers
// branch to a fresh run instead of assuming every backend can wake a
// checkpoint.
func TestAsResumerReportsFalseForExecuteOnlyAgent(t *testing.T) {
	var agent Agent[*testRequest, *testResponse, testCallbacks] = executeOnlyAgent{}
	if _, ok := AsResumer[*testRequest](agent); ok {
		t.Error("AsResumer() = true for an Execute-only agent, want false")
	}
}

// TestAsResumerReportsTrueForResumerAgent is the positive twin over a plain
// fake, pinning that the assertion keys on the Resume method alone.
func TestAsResumerReportsTrueForResumerAgent(t *testing.T) {
	var agent Agent[*testRequest, *testResponse, testCallbacks] = resumableAgent{}
	if _, ok := AsResumer[*testRequest](agent); !ok {
		t.Error("AsResumer() = false for a Resumer-implementing agent, want true")
	}
}

// executeOnlyAgent implements Agent but NOT Resumer.
type executeOnlyAgent struct{}

func (executeOnlyAgent) Execute(context.Context, *testRequest, testCallbacks) (*testResponse, error) {
	return nil, errors.New("not implemented")
}

// resumableAgent implements both Agent and Resumer.
type resumableAgent struct{ executeOnlyAgent }

func (resumableAgent) Resume(context.Context, checkpoint.Envelope, map[string]string, testCallbacks) (*testResponse, error) {
	return nil, errors.New("not implemented")
}
