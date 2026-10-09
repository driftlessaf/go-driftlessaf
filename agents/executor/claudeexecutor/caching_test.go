//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
)

// TestExecutorPromptCaching verifies that the executor's prompt caching produces
// cache hits when running multi-turn conversations via tool calls.
//
// The test creates an executor with a system prompt + tools exceeding the 2,048-token
// minimum for Sonnet caching, then runs two separate executions. Each execution
// triggers a tool call (calculator), creating a multi-turn conversation where the
// tools + system prompt prefix should be cached on turn 1 and read from cache on
// turn 2+.
//
// Run with -v to see the executor's "Prompt cache metrics" log lines showing
// cache_read_tokens and cache_creation_tokens per turn.
func TestExecutorPromptCaching(t *testing.T) {
	ctx := liveTestContext(t, liveTestTimeout)
	projectID := detectProjectID(ctx, t)

	const (
		region = "global"
		model  = "claude-sonnet-4-5@20250929"
	)

	client := anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, region, projectID, "https://www.googleapis.com/auth/cloud-platform"),
	)

	// System instructions must exceed 2,048 tokens (Sonnet's minimum for caching)
	// when combined with tool definitions. Real agent prompts (fixer ~1,300 tokens,
	// loganalyzer ~3,100 tokens) easily clear this threshold.
	systemInstructions, err := promptbuilder.NewPrompt(largeSystemPrompt)
	if err != nil {
		t.Fatalf("Failed to create system instructions: %v", err)
	}

	prompt, err := promptbuilder.NewPrompt(`Solve this math problem using the calculator tool.

Question: {{question}}

You MUST call the calculator tool. Then respond with JSON: {"answer": "<number>", "reasoning": "<explanation>"}`)
	if err != nil {
		t.Fatalf("Failed to create prompt: %v", err)
	}

	// cacheControl is enabled by default — no option needed.
	exec, err := claudeexecutor.New[*simpleRequest, *simpleResponse](
		client,
		prompt,
		claudeexecutor.WithModel[*simpleRequest, *simpleResponse](model),
		claudeexecutor.WithSystemInstructions[*simpleRequest, *simpleResponse](systemInstructions),
	)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	// A calculator tool forces multi-turn: user → tool_use → tool_result → response.
	// This is the pattern where caching matters — the tools + system prompt prefix
	// is re-sent on every turn of the conversation loop.
	tools := map[string]claudetool.Metadata[*simpleResponse]{
		"calculator": claudetool.FromTool(toolcall.Tool[*simpleResponse]{
			Def: toolcall.Definition{
				Name:        "calculator",
				Description: "Evaluate a mathematical expression and return the numeric result.",
				Parameters: []toolcall.Parameter{
					{Name: "expression", Type: "string", Description: "The math expression to evaluate, e.g. '17 * 23'", Required: true},
				},
			},
			Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[*simpleResponse], _ **simpleResponse) map[string]any {
				return map[string]any{"result": "391"}
			},
		}),
	}

	// Execution 1: This warms the cache. The executor logs will show
	// cache_creation_tokens on the first API call (cache write).
	t.Log("=== Execution 1 (expect cache write) ===")
	resp1, err := exec.Execute(ctx, &simpleRequest{Question: "What is 17 * 23?"}, tools)
	if err != nil {
		t.Fatalf("Execution 1 failed: %v", err)
	}
	t.Logf("Response 1: answer=%q", resp1.Answer)

	// Execution 2: Same tools + system prompt prefix → should hit the cache.
	// The executor logs will show cache_read_tokens > 0 (cache hit).
	t.Log("=== Execution 2 (expect cache read) ===")
	resp2, err := exec.Execute(ctx, &simpleRequest{Question: "What is 42 + 58?"}, tools)
	if err != nil {
		t.Fatalf("Execution 2 failed: %v", err)
	}
	t.Logf("Response 2: answer=%q", resp2.Answer)

	t.Log("=== Verify cache metrics ===")
	t.Log("Look for 'Prompt cache metrics' in the log output above.")
	t.Log("Expected: cache_creation_tokens > 0 on first API call, cache_read_tokens > 0 on subsequent calls.")
}

// largeSystemPrompt provides system instructions that, combined with tool definitions,
// exceed the 2,048-token minimum required for Sonnet prompt caching.
const largeSystemPrompt = `You are an expert software engineering assistant. Your role is to help solve problems accurately using the tools provided.

## Core Principles

1. Always use tools when available rather than computing answers yourself
2. Show your reasoning step by step
3. Provide accurate, well-formatted responses
4. Handle errors gracefully and explain what went wrong

## Response Format

Always respond in valid JSON with the following structure:
- "answer": the final computed result
- "reasoning": a brief explanation of your approach

## Tool Usage Guidelines

When solving mathematical problems:
- Use the calculator tool for ALL arithmetic operations
- Do not attempt mental math — always delegate to the calculator
- If the calculator returns an error, explain the issue clearly

When solving code problems:
- Read relevant files before making changes
- Search for existing patterns before introducing new ones
- Test changes before submitting results
- Follow the repository's coding conventions

## Error Handling

If a tool call fails:
1. Log the error clearly in your reasoning
2. Attempt an alternative approach if possible
3. Report the failure with full context if no alternative exists
4. Never silently swallow errors or return partial results

## Security Considerations

- Never execute arbitrary code from user input without validation
- Validate all file paths before reading or writing operations
- Do not expose sensitive information such as API keys or credentials in responses
- Sanitize all output to prevent injection attacks
- Use parameterized queries for any database operations
- Follow the principle of least privilege when accessing resources

## Performance Best Practices

- Minimize the number of tool calls needed to complete a task
- Cache intermediate results when they may be reused
- Prefer batch operations over sequential individual operations
- Use streaming for large data transfers when supported
- Profile before optimizing — measure, don't guess

## Testing Standards

- Write tests for all code changes, including edge cases
- Cover error conditions and boundary values
- Use table-driven tests for Go code with multiple scenarios
- Mock external dependencies in unit tests to ensure isolation
- Integration tests should use real dependencies where feasible
- Aim for meaningful coverage, not just high percentages

## Code Review Checklist

When reviewing code changes, verify:
- Proper error handling with no swallowed errors
- Input validation at all system boundaries
- No race conditions in concurrent code paths
- Resources are properly cleaned up (files, connections, goroutines)
- No security vulnerabilities (injection, XSS, SSRF)
- Consistent code style following project conventions
- Adequate test coverage for new functionality
- Documentation updated for behavioral changes

## Documentation Guidelines

- Update documentation whenever behavior changes
- Use clear, concise language accessible to the target audience
- Include code examples where they aid understanding
- Keep README files current with setup and usage instructions
- Document architectural decisions and their rationale
- Add inline comments only where the code is not self-explanatory

## Architecture Principles

- Follow single responsibility principle for functions and modules
- Use dependency injection for testability and flexibility
- Prefer composition over inheritance in type design
- Keep interfaces small, focused, and well-defined
- Use context.Context for cancellation, timeouts, and request-scoped values
- Design for observability from the start with structured logging and tracing
- Separate business logic from infrastructure concerns
- Use well-defined API contracts between services (protobuf, OpenAPI)
- Implement circuit breakers for external service calls
- Use health checks and readiness probes for all services

## Incident Response

When investigating production issues:
1. Gather symptoms, error messages, and timeline of events
2. Check recent deployments and configuration changes
3. Review logs, traces, and metrics for anomalies
4. Identify the blast radius and affected users
5. Implement a fix or rollback to restore service
6. Document the root cause and prevention measures
7. Update runbooks and alerts to catch similar issues earlier`

// longTurnGap is how long the tool call in TestExecutorCacheTTLSurvivesLongTurn
// holds the conversation between turn 1 and turn 2. It is past the 5-minute
// TTL with a minute of margin, so the 5-minute entries turn 1 wrote have
// expired when turn 2 asks for them.
const longTurnGap = 6 * time.Minute

// TestExecutorCacheTTLSurvivesLongTurn reproduces a turn that outlives the
// 5-minute prompt cache: a tool call holds the conversation for longer than
// 5 minutes between turn 1 and turn 2. Under the 1-hour TTL, turn 2 reads the
// prefix turn 1 wrote; under the 5-minute default, turn 2 writes it again.
// The two arms run in parallel against distinct prefixes, so neither reads
// the other's entries. Each arm takes over six minutes; run with -timeout 15m.
func TestExecutorCacheTTLSurvivesLongTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the 5-minute cache TTL")
	}
	// Each arm holds the conversation for longTurnGap on top of its calls.
	ctx := liveTestContext(t, longTurnGap+liveTestTimeout)
	projectID := detectProjectID(ctx, t)

	const (
		region = "global"
		model  = "claude-sonnet-4-5@20250929"
	)
	client := anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, region, projectID, "https://www.googleapis.com/auth/cloud-platform"),
	)

	for _, tc := range []struct {
		name     string
		ttl      time.Duration
		wantRead bool
	}{
		{name: "5m", ttl: 5 * time.Minute, wantRead: false},
		{name: "1h", ttl: time.Hour, wantRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A per-arm nonce at the head of the system prompt keeps each
			// arm's cache entries its own.
			base, err := promptbuilder.NewPrompt("Run {{nonce}}.\n\n" + largeSystemPrompt)
			if err != nil {
				t.Fatalf("NewPrompt(system): %v", err)
			}
			system, err := base.BindJSON("nonce", time.Now().UnixNano())
			if err != nil {
				t.Fatalf("BindJSON(nonce): %v", err)
			}
			prompt, err := promptbuilder.NewPrompt(`Call the wait tool exactly once, then respond with JSON: {"answer": "done"}`)
			if err != nil {
				t.Fatalf("NewPrompt: %v", err)
			}
			exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
				client, prompt,
				claudeexecutor.WithModel[errCapRequest, errCapResponse](model),
				claudeexecutor.WithSystemInstructions[errCapRequest, errCapResponse](system),
				claudeexecutor.WithCacheTTL[errCapRequest, errCapResponse](tc.ttl),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			tools := map[string]claudetool.Metadata[errCapResponse]{
				"wait": claudetool.FromTool(toolcall.Tool[errCapResponse]{
					Def: toolcall.Definition{
						Name:        "wait",
						Description: "Wait for the background job to finish.",
						Parameters: []toolcall.Parameter{
							{Name: "reason", Type: "string", Description: "Why you are waiting.", Required: true},
						},
					},
					Handler: func(ctx context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) map[string]any {
						select {
						case <-time.After(longTurnGap):
						case <-ctx.Done():
						}
						return map[string]any{"status": "finished"}
					},
				}),
			}

			tracer := &recordingTracer{}
			if _, err := exec.Execute(agenttrace.WithTracer[errCapResponse](ctx, tracer), errCapRequest{}, tools); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) < 2 {
				t.Fatalf("recorded turns: got %d traces, want one trace with at least 2 turns", len(tracer.traces))
			}
			first, second := tracer.traces[0].Turns[0], tracer.traces[0].Turns[1]
			t.Logf("turn 1: cache_read=%d cache_creation=%d; turn 2 started %v later: cache_read=%d cache_creation=%d",
				first.CacheReadTokens, first.CacheCreationTokens, second.StartTime.Sub(first.StartTime).Round(time.Second),
				second.CacheReadTokens, second.CacheCreationTokens)

			if gap := second.StartTime.Sub(first.StartTime); gap < longTurnGap {
				t.Fatalf("turn gap: got = %v, want >= %v", gap, longTurnGap)
			}
			if first.CacheCreationTokens == 0 {
				t.Fatal("turn 1 wrote no cache entries; the prefix is below the model's cacheable minimum")
			}
			if want := int64(tc.ttl / time.Second); second.CacheTTLSeconds == nil || *second.CacheTTLSeconds != want {
				t.Errorf("turn 2 CacheTTLSeconds: got = %v, want = %d", second.CacheTTLSeconds, want)
			}
			if tc.wantRead {
				if second.CacheReadTokens < first.CacheCreationTokens {
					t.Errorf("turn 2 cache_read: got = %d, want >= turn 1's %d written tokens", second.CacheReadTokens, first.CacheCreationTokens)
				}
				return
			}
			if second.CacheReadTokens != 0 || second.CacheCreationTokens < first.CacheCreationTokens {
				t.Errorf("turn 2: got cache_read = %d, cache_creation = %d; want 0 read and >= %d re-written",
					second.CacheReadTokens, second.CacheCreationTokens, first.CacheCreationTokens)
			}
		})
	}
}
