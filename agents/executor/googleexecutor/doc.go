/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package googleexecutor provides a generic Google AI (Gemini) executor for AI agents.

This package implements a reusable pattern for Google AI-based agents, handling:
  - Prompt template rendering
  - Chat session management
  - Tool/function calling
  - Response parsing and extraction
  - Trace management for evaluation

# Architecture

The executor follows a generic design pattern where Request and Response types
are parameterized, allowing different agents to reuse the same core logic:

	type MyRequest struct {
	    Input string
	}

	type MyResponse struct {
	    Output string
	}

	executor, err := googleexecutor.New[*MyRequest, *MyResponse](
	    client,
	    promptTemplate,
	    googleexecutor.WithModel[*MyRequest, *MyResponse]("gemini-2.5-flash"),
	)

# Tool Support

The executor supports Google AI function calling through the Metadata type:

	tools := map[string]googletool.Metadata[*MyResponse]{
	    "my_tool": {
	        Definition: &genai.FunctionDeclaration{
	            Name:        "my_tool",
	            Description: "Tool description",
	            Parameters: &genai.Schema{...},
	        },
	        Handler: func(ctx context.Context, call *genai.FunctionCall, trace *evals.Trace[*MyResponse]) *genai.FunctionResponse {
	            // Tool implementation
	        },
	    },
	}

	response, err := executor.Execute(ctx, request, tools)

# Options

The executor supports various configuration options:

  - WithModel: Set the Gemini model to use
  - WithTemperature: Control response randomness (0.0-2.0)
  - WithMaxOutputTokens: Set maximum response length
  - WithSystemInstructions: Provide system-level instructions
  - WithResponseMIMEType: Set response format (e.g., "application/json")
  - WithResponseSchema: Define structured output schema
  - WithThinking: Enable thinking mode with a token budget

# Thinking Mode

Thinking mode allows Gemini to show its internal reasoning process. When enabled,
thought blocks are captured in the trace:

	executor, err := googleexecutor.New[*Request, *Response](
	    client,
	    prompt,
	    googleexecutor.WithThinking[*Request, *Response](2048), // 2048 token budget for thinking
	)

Reasoning blocks are stored in trace.Reasoning as []evals.ReasoningContent,
where each block contains:
  - Thinking: the reasoning text

# Integration with Evaluation

The executor automatically integrates with the evals package for tracing:
  - Creates traces for each execution
  - Records tool calls and responses
  - Tracks bad tool calls for debugging
  - Provides complete execution history

# Error Handling

The executor provides comprehensive error handling:
  - Template rendering errors
  - Chat creation failures
  - Malformed function calls (with automatic retry)
  - Response parsing errors
  - Tool execution errors

# Usage Example

	// Create client
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
	    Project:  projectID,
	    Location: region,
	    Backend:  genai.BackendVertexAI,
	})

	// Parse template
	tmpl := template.Must(template.New("prompt").Parse("Analyze: {{.Input}}"))

	// Create executor
	executor, err := googleexecutor.New[*Request, *Response](
	    client,
	    tmpl,
	    googleexecutor.WithModel[*Request, *Response]("gemini-2.5-flash"),
	    googleexecutor.WithTemperature[*Request, *Response](0.1),
	    googleexecutor.WithResponseMIMEType[*Request, *Response]("application/json"),
	)

	// Execute
	response, err := executor.Execute(ctx, request, nil)

# Performance Considerations

  - Templates are executed for each request (consider pre-rendering if static)
  - Chat sessions are created per execution (not reused)
  - Tool responses are sent synchronously
  - Large response schemas may impact latency

# Thread Safety

The executor is safe for concurrent use. Each Execute call creates its own
chat session and maintains independent state.
*/
package googleexecutor
