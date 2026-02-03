//go:build withauth

/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
)

// simpleRequest implements promptbuilder.Bindable for testing
type simpleRequest struct {
	Question string
}

func (r *simpleRequest) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	// Bind question as XML to safely handle user input
	return p.BindXML("question", struct {
		XMLName struct{} `xml:"question"`
		Content string   `xml:",chardata"`
	}{
		Content: r.Question,
	})
}

// simpleResponse is the expected JSON response format
type simpleResponse struct {
	Answer    json.Number `json:"answer"`
	Reasoning string      `json:"reasoning"`
}

// detectProjectID tries to detect the GCP project ID from environment
func detectProjectID(ctx context.Context, t *testing.T) string {
	// Try various environment variables
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		projectID = os.Getenv("GCP_PROJECT")
	}
	if projectID == "" {
		projectID = os.Getenv("GCLOUD_PROJECT")
	}
	if projectID == "" {
		t.Skip("Skipping integration test: no GCP project ID found in environment (set GOOGLE_CLOUD_PROJECT, GCP_PROJECT, or GCLOUD_PROJECT)")
	}
	return projectID
}

func TestExecutorWithThinking(t *testing.T) {
	ctx := context.Background()

	if testing.Short() {
		// https://github.com/anthropics/anthropic-sdk-go/issues/222
		t.Skip("Skipping anthropic test in short mode.")
	}

	// Detect project ID
	projectID := detectProjectID(ctx, t)

	// Create client with Vertex AI authentication
	client := anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, "us-east5", projectID),
	)

	// Create prompt template
	prompt, err := promptbuilder.NewPrompt(`You are a helpful math assistant.

Question: {{question}}

Please solve this problem and provide your answer in JSON format:
{
  "answer": "the numerical answer",
  "reasoning": "brief explanation of how you solved it"
}`)
	if err != nil {
		t.Fatalf("Failed to create prompt: %v", err)
	}

	// Create executor with thinking enabled
	exec, err := claudeexecutor.New[*simpleRequest, *simpleResponse](
		client,
		prompt,
		claudeexecutor.WithModel[*simpleRequest, *simpleResponse]("claude-sonnet-4@20250514"),
		claudeexecutor.WithMaxTokens[*simpleRequest, *simpleResponse](8192),
		claudeexecutor.WithThinking[*simpleRequest, *simpleResponse](2048), // Enable thinking with modest budget
	)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	// Create namespaced observer
	obs := evals.NewNamespacedObserver(func(name string) *mockObserver {
		return &mockObserver{}
	})

	// Create eval callback that validates reasoning blocks
	reasoningValidator := func(o evals.Observer, trace *evals.Trace[*simpleResponse]) {
		if len(trace.Reasoning) == 0 {
			o.Fail("no reasoning blocks captured in trace")
			return
		}

		// Verify first block has expected content
		first := trace.Reasoning[0]
		if first.Thinking == "" {
			o.Fail("reasoning block missing thinking")
			return
		}

		o.Log(fmt.Sprintf("Captured %d reasoning block(s), first has %d chars",
			len(trace.Reasoning), len(first.Thinking)))
	}

	// Build tracer with eval callback
	tracer := evals.BuildTracer(obs, map[string]evals.ObservableTraceCallback[*simpleResponse]{
		"reasoning_validator": reasoningValidator,
	})
	ctx = evals.WithTracer(ctx, tracer)

	// Execute with a simple math problem that should trigger thinking
	request := &simpleRequest{
		Question: "What is 17 * 23?",
	}

	response, err := exec.Execute(ctx, request, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Verify we got a valid response
	if response == nil {
		t.Fatal("Expected non-nil response")
	}

	if response.Answer == "" {
		t.Error("Expected non-empty answer")
	}

	t.Logf("Response: answer=%q, reasoning=%q", response.Answer, response.Reasoning)

	// Check if any eval failures occurred by inspecting the observer
	var failures []string
	var logs []string
	obs.Walk(func(name string, o *mockObserver) {
		failures = append(failures, o.failures...)
		logs = append(logs, o.logs...)
	})

	if len(failures) > 0 {
		t.Errorf("Thinking validation failed:\n%s", strings.Join(failures, "\n"))
	}

	// Log all eval logs
	for _, log := range logs {
		t.Log(log)
	}
}

// mockObserver implements evals.Observer for testing
type mockObserver struct {
	failures []string
	logs     []string
}

func (m *mockObserver) Fail(msg string) {
	m.failures = append(m.failures, msg)
}

func (m *mockObserver) Log(msg string) {
	m.logs = append(m.logs, msg)
}

func (m *mockObserver) Grade(score float64, reasoning string) {
	m.logs = append(m.logs, fmt.Sprintf("Grade: %.2f - %s", score, reasoning))
}

func (m *mockObserver) Increment() {}

func (m *mockObserver) Total() int64 {
	return 0
}
