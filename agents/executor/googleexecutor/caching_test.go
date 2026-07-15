//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// TestExecutorContextCaching verifies that the executor's Vertex AI context
// caching produces cache hits when running separate executions with the same
// system prompt and tools.
//
// The test creates an executor with a large system prompt + tools, then runs
// two separate executions. The first execution creates the CachedContent
// resource; the second references it and should report CachedContentTokenCount > 0
// in the response metadata.
//
// Run with -v to see the executor's "Prompt cache metrics" and
// "Created context cache" log lines.
func TestExecutorContextCaching(t *testing.T) {
	ctx := t.Context()
	projectID := detectProjectID(ctx, t)
	model := getTestModel()
	t.Logf("Using model: %s", model)

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  projectID,
		Location: "global",
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// System instructions must be large enough for Vertex AI context caching.
	// The minimum varies by model (e.g., 32,768 tokens for Gemini 1.5).
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

	// cacheControl is enabled by default.
	exec, err := googleexecutor.New[*simpleRequest, *simpleResponse](
		client,
		prompt,
		googleexecutor.WithModel[*simpleRequest, *simpleResponse](model),
		googleexecutor.WithSystemInstructions[*simpleRequest, *simpleResponse](systemInstructions),
		// cacheControl is enabled by default — no option needed.
	)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	// A calculator tool forces multi-turn: user -> function_call -> function_response -> text.
	tools := map[string]googletool.Metadata[*simpleResponse]{
		"calculator": googletool.FromTool(toolcall.Tool[*simpleResponse]{
			Def: toolcall.Definition{
				Name:        "calculator",
				Description: "Evaluate a mathematical expression and return the numeric result.",
				Parameters: []toolcall.Parameter{
					{Name: "expression", Type: "string", Description: "The math expression to evaluate, e.g. '17 * 23'", Required: true},
				},
			},
			Handler: func(_ context.Context, tc toolcall.ToolCall, _ *agenttrace.Trace[*simpleResponse], _ **simpleResponse) map[string]any {
				// Evaluate the expression for real. The tool exists only to
				// force a multi-turn exchange (user -> function_call ->
				// function_response -> text), and a canned result ("391",
				// correct for execution 1 but wrong for execution 2's
				// 42+58) used to slip through — until models started
				// refusing to repeat a tool result they can see is wrong,
				// which puts refusal prose in "answer" and breaks the
				// json.Number parse. Keep the fixture honest so the test
				// exercises caching, not model alignment.
				expr, _ := tc.Args["expression"].(string)
				return map[string]any{"result": evalSimpleExpr(expr)}
			},
		}),
	}

	// Execution 1: basic execution without caching (validates plumbing).
	t.Log("=== Execution 1 ===")
	resp1, err := exec.Execute(ctx, &simpleRequest{Question: "What is 17 * 23?"}, tools)
	if err != nil {
		t.Fatalf("Execution 1 failed: %v", err)
	}
	t.Logf("Response 1: answer=%q", resp1.Answer)

	// Execution 2: second execution.
	t.Log("=== Execution 2 ===")
	resp2, err := exec.Execute(ctx, &simpleRequest{Question: "What is 42 + 58?"}, tools)
	if err != nil {
		t.Fatalf("Execution 2 failed: %v", err)
	}
	t.Logf("Response 2: answer=%q", resp2.Answer)

	t.Log("=== Done ===")
	t.Log("When caching is enabled with a model that supports context caching,")
	t.Log("look for 'Created context cache' and 'Prompt cache metrics' in the log output.")
}

// largeSystemPrompt provides system instructions large enough for context caching.
// Vertex AI requires a minimum of 1,024 tokens for context caching to activate
// on gemini-2.5-flash. This prompt is designed to comfortably exceed that threshold.
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
7. Update runbooks and alerts to catch similar issues earlier

## Dependency Management

- Pin exact versions for critical dependencies to ensure reproducible builds
- Use Go module replace directives for local development
- Run go mod tidy after any dependency changes
- Review transitive dependency updates for security vulnerabilities
- Keep dependencies up to date with automated tools like Dependabot
- Prefer stdlib solutions over third-party when functionality is equivalent
- Document any non-obvious dependency choices in comments

## Observability Standards

- Emit structured logs (JSON format) at appropriate levels
- Use distributed tracing with context propagation across service boundaries
- Define SLIs and SLOs for all user-facing services
- Set up dashboards for key metrics before launching features
- Implement alerting with clear runbooks for each alert
- Use metric labels consistently across services for cross-service debugging
- Record business metrics alongside technical metrics for complete visibility
- Ensure logs include correlation IDs for request tracing

## Deployment and Rollout

- Use canary deployments for high-risk changes
- Monitor error rates and latency during rollouts
- Have automated rollback triggers for critical metrics
- Test configuration changes in staging before production
- Use feature flags for gradual rollouts of new functionality
- Document deployment procedures in runbooks
- Verify health checks pass before routing traffic to new instances`

// evalSimpleExpr evaluates "a op b" for the toy calculator fixture. Unknown
// shapes return the input unchanged, which surfaces clearly in the trace.
func evalSimpleExpr(expr string) string {
	f := strings.Fields(expr)
	if len(f) != 3 {
		return expr
	}
	a, errA := strconv.Atoi(f[0])
	b, errB := strconv.Atoi(f[2])
	if errA != nil || errB != nil {
		return expr
	}
	switch f[1] {
	case "+":
		return strconv.Itoa(a + b)
	case "-":
		return strconv.Itoa(a - b)
	case "*":
		return strconv.Itoa(a * b)
	default:
		return expr
	}
}
