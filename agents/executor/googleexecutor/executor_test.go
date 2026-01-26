//go:build withauth

/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"google.golang.org/genai"
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
	Answer    string `json:"answer"`
	Reasoning string `json:"reasoning"`
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

	// Detect project ID
	projectID := detectProjectID(ctx, t)

	// Create Gemini client
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  projectID,
		Location: "us-central1",
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

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
	exec, err := googleexecutor.New[*simpleRequest, *simpleResponse](
		client,
		prompt,
		googleexecutor.WithModel[*simpleRequest, *simpleResponse]("gemini-2.5-flash"),
		googleexecutor.WithMaxOutputTokens[*simpleRequest, *simpleResponse](8192),
		googleexecutor.WithThinking[*simpleRequest, *simpleResponse](2048), // Enable thinking with modest budget
		googleexecutor.WithResponseMIMEType[*simpleRequest, *simpleResponse]("application/json"),
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
			o.Fail("reasoning block has empty thinking")
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
		t.Errorf("Reasoning validation failed:\n%s", strings.Join(failures, "\n"))
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
