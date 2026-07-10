/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package claudeexecutor provides a generic executor for Claude-based agents that
// reduces boilerplate while maintaining flexibility for agent-specific logic.
//
// The executor handles the common conversation loop pattern including:
//   - Prompt rendering from templates
//   - Message streaming and accumulation
//   - Tool call execution and response handling
//   - JSON response parsing
//   - Trace management for evaluation
//
// # Basic Usage
//
// Create an executor with a client and prompt template:
//
//	client := anthropic.NewClient(
//	    vertex.WithGoogleAuth(ctx, region, projectID, "https://www.googleapis.com/auth/cloud-platform"),
//	)
//
//	tmpl, _ := template.New("prompt").Parse("Analyze: {{.Input}}")
//
//	exec, err := claudeexecutor.New[*Request, *Response](
//	    client,
//	    tmpl,
//	    claudeexecutor.WithModel[*Request, *Response]("claude-3-opus@20240229"),
//	    claudeexecutor.WithMaxTokens[*Request, *Response](16000),
//	)
//	if err != nil {
//	    return nil, err
//	}
//
//	// Define tools if needed
//	tools := map[string]claudetool.Metadata[*Response]{
//	    "read_file": {
//	        Definition: anthropic.ToolParam{
//	            Name:        "read_file",
//	            Description: anthropic.String("Read a file"),
//	            InputSchema: anthropic.ToolInputSchemaParam{
//	                Properties: map[string]interface{}{
//	                    "path": map[string]interface{}{
//	                        "type": "string",
//	                        "description": "File path",
//	                    },
//	                },
//	                Required: []string{"path"},
//	            },
//	        },
//	        Handler: func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[*Response]) map[string]interface{} {
//	            // Tool implementation
//	            return map[string]interface{}{"content": "file contents"}
//	        },
//	    },
//	}
//
//	// Execute the agent
//	response, err := exec.Execute(ctx, request, tools)
//
// # Options
//
// The executor supports several configuration options:
//   - WithModel: Override the default model (defaults to claude-sonnet-4@20250514)
//   - WithMaxTokens: Set maximum response tokens (defaults to 8192, max 32000)
//   - WithTemperature: Set response temperature (defaults to 0.1)
//   - WithSystemInstructions: Provide system-level instructions
//   - WithThinking: Enable extended thinking mode with a token budget
//   - WithCacheFirstUserBlock: Also cache the first user message (off by default)
//   - WithMaxToolCallsBeforeFinalize: Soft-cap the agentic loop (off by default)
//   - WithForceSubmitToolChoice: Force the terminal submit tool via tool_choice (off by default)
//
// # Prompt Caching
//
// Anthropic prompt caching is enabled by default (disable with
// WithoutCacheControl). The executor places cache breakpoints on the static
// request prefix — the last tool definition and the system prompt — and
// advances a moving breakpoint along the conversation tail each turn. The
// tail breakpoint means the accumulated message history (the rendered prompt
// and every prior tool result) is written to the cache once and read at the
// cached-token price on subsequent turns, instead of being re-billed at the
// full input price on every turn of the loop. Two tail markers are retained
// (current and previous) so a turn that appends many blocks — for example
// several parallel tool calls — still resumes from the prior turn's cache
// entry. Cache reads and writes are reported per turn via the
// gen_ai.usage.cache_* metrics and on the trace.
//
// # Extended Thinking
//
// Extended thinking allows Claude to show its internal reasoning process before
// responding. When enabled, reasoning blocks are captured in the trace:
//
//	exec, err := claudeexecutor.New[*Request, *Response](
//	    client,
//	    prompt,
//	    claudeexecutor.WithThinking[*Request, *Response](2048), // 2048 token budget for thinking
//	)
//
// Reasoning blocks are stored in trace.Reasoning as []agenttrace.ReasoningContent,
// where each block contains:
//   - Thinking: the reasoning text
//
// Note: When thinking is enabled, temperature is automatically set to 1.0 as required
// by the Claude API. See: https://docs.claude.com/en/docs/build-with-claude/extended-thinking
//
// # Claude Opus 4.7 Compatibility
//
// Opus 4.7 introduced two breaking changes that the executor handles transparently
// so callers don't need model-aware logic:
//
//   - Sampling parameters (temperature, top_p, top_k) are rejected with a 400.
//     WithTemperature is silently dropped for Opus 4.7 models; a warning is logged
//     once per Execute if the caller explicitly set it.
//   - Extended-thinking budgets are replaced by adaptive thinking. WithThinking(N)
//     is mapped to adaptive thinking for Opus 4.7 (the budget is advisory to the
//     model via adaptive mode). A warning is logged once per Execute noting the
//     mapping. Display is set to "summarized" so reasoning traces remain populated.
//
// See: https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7
//
// # Type Safety
//
// The executor is generic over Request and Response types, ensuring type safety
// throughout the conversation. The trace parameter in tool handlers is properly
// typed with the Response type.
package claudeexecutor
