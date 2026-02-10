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
//	    vertex.WithGoogleAuth(ctx, region, projectID),
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
// # Type Safety
//
// The executor is generic over Request and Response types, ensuring type safety
// throughout the conversation. The trace parameter in tool handlers is properly
// typed with the Response type.
package claudeexecutor
