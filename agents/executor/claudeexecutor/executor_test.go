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

	"chainguard.dev/driftlessaf/agents/agenttrace"
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
	ctx := t.Context()

	// Detect project ID
	projectID := detectProjectID(ctx, t)

	// Define base model test configurations (Claude Haiku for fast testing)
	tests := []struct {
		name     string
		region   string
		model    string
		thinking int64 // 0 means no thinking
	}{{
		name:   "claude-haiku-4-5",
		region: "us-east5",
		model:  "claude-haiku-4-5@20251001",
	}}

	// Add Claude Sonnet with extended thinking when not in short mode
	if !testing.Short() {
		tests = append(tests, struct {
			name     string
			region   string
			model    string
			thinking int64
		}{
			name:     "claude-sonnet-4-thinking",
			region:   "us-east5",
			model:    "claude-sonnet-4-5@20250929",
			thinking: 2048,
		})
	}

	t.Logf("Running %d test configurations (testing.Short=%v)", len(tests), testing.Short())

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create client with Vertex AI authentication
			client := anthropic.NewClient(
				vertex.WithGoogleAuth(ctx, tt.region, projectID, "https://www.googleapis.com/auth/cloud-platform"),
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

			execOpts := []claudeexecutor.Option[*simpleRequest, *simpleResponse]{
				claudeexecutor.WithModel[*simpleRequest, *simpleResponse](tt.model),
				claudeexecutor.WithMaxTokens[*simpleRequest, *simpleResponse](8192),
			}
			if tt.thinking > 0 {
				execOpts = append(execOpts, claudeexecutor.WithThinking[*simpleRequest, *simpleResponse](tt.thinking))
			}

			exec, err := claudeexecutor.New[*simpleRequest, *simpleResponse](
				client,
				prompt,
				execOpts...,
			)
			if err != nil {
				t.Fatalf("Failed to create executor: %v", err)
			}

			testCtx := ctx
			// Set up thinking validation when thinking is enabled
			if tt.thinking > 0 {
				obs := evals.NewNamespacedObserver(func(name string) *mockObserver {
					return &mockObserver{}
				})

				reasoningValidator := func(o evals.Observer, trace *agenttrace.Trace[*simpleResponse]) {
					if len(trace.Reasoning) == 0 {
						o.Fail("no reasoning blocks captured in trace")
						return
					}

					first := trace.Reasoning[0]
					if first.Thinking == "" {
						o.Fail("reasoning block missing thinking")
						return
					}

					o.Log(fmt.Sprintf("Captured %d reasoning block(s), first has %d chars",
						len(trace.Reasoning), len(first.Thinking)))
				}

				tracer := evals.BuildTracer(obs, map[string]evals.ObservableTraceCallback[*simpleResponse]{
					"reasoning_validator": reasoningValidator,
				})
				// Reasoning capture is gated behind the payload opt-in, so enable
				// it here — otherwise trace.Reasoning stays empty and the validator
				// fails.
				testCtx = agenttrace.WithPayloadsEnabled(testCtx, true)
				testCtx = agenttrace.WithTracer(testCtx, tracer)

				t.Cleanup(func() {
					var failures []string
					var logs []string
					obs.Walk(func(name string, o *mockObserver) {
						failures = append(failures, o.failures...)
						logs = append(logs, o.logs...)
					})

					if len(failures) > 0 {
						t.Errorf("Thinking validation failed:\n%s", strings.Join(failures, "\n"))
					}

					for _, log := range logs {
						t.Log(log)
					}
				})
			}

			request := &simpleRequest{
				Question: "What is 17 * 23?",
			}

			response, err := exec.Execute(testCtx, request, nil)
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}

			if response == nil {
				t.Fatal("Expected non-nil response")
			}

			if response.Answer == "" {
				t.Error("Expected non-empty answer")
			}

			t.Logf("Response: answer=%q, reasoning=%q", response.Answer, response.Reasoning)
		})
	}
}

// TestOpus47WithSamplingParamsAndThinking verifies the claudeexecutor is
// compatible with Claude Opus 4.7, which (a) rejects the sampling-param fields
// (temperature, top_p, top_k) with a 400, and (b) replaced extended-thinking
// budgets with adaptive thinking. The executor must drop temperature and map
// WithThinking to adaptive mode. A single Execute hitting the live API proves
// the request is accepted end-to-end.
func TestOpus47WithSamplingParamsAndThinking(t *testing.T) {
	ctx := t.Context()
	projectID := detectProjectID(ctx, t)

	// Opus 4.7 is served via the global Vertex endpoint.
	const model = "claude-opus-4-7@default"
	const region = "global"

	client := anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, region, projectID, "https://www.googleapis.com/auth/cloud-platform"),
	)

	prompt, err := promptbuilder.NewPrompt(`Reply in JSON: {"answer":"<short>","reasoning":"<one sentence>"}.
Question: {{question}}`)
	if err != nil {
		t.Fatalf("Failed to create prompt: %v", err)
	}

	exec, err := claudeexecutor.New[*simpleRequest, *simpleResponse](
		client,
		prompt,
		claudeexecutor.WithModel[*simpleRequest, *simpleResponse](model),
		claudeexecutor.WithMaxTokens[*simpleRequest, *simpleResponse](4096),
		// Both must be tolerated on 4.7: temperature is silently dropped, and
		// WithThinking is mapped to adaptive thinking.
		claudeexecutor.WithTemperature[*simpleRequest, *simpleResponse](0.2),
		claudeexecutor.WithThinking[*simpleRequest, *simpleResponse](2048),
	)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	resp, err := exec.Execute(ctx, &simpleRequest{Question: "What is 2 + 2?"}, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if resp == nil || resp.Answer == "" {
		t.Fatalf("Expected non-empty answer, got %+v", resp)
	}
	t.Logf("Opus 4.7 answered: %q", resp.Answer)
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
