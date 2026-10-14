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
	    googleexecutor.WithModel[*MyRequest, *MyResponse]("gemini-3.8-flash"),
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
	        Handler: func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[*MyResponse]) *genai.FunctionResponse {
	            // Tool implementation
	        },
	    },
	}

	response, err := executor.Execute(ctx, request, tools)

# Options

The executor supports various configuration options:

  - WithAttribution: Set canonical and compatibility route attribution
  - WithModel: Set the Gemini model to use
  - WithRoutedModel: Set separate provider-wire and logical capability model IDs
  - WithTemperature: Control response randomness (0.0-2.0)
  - WithoutTemperature: Omit temperature when an explicit route disallows sampling parameters
  - WithMaxOutputTokens: Set maximum response length
  - WithSystemInstructions: Provide system-level instructions
  - WithUserPromptSuffix: Append a static prompt to the built user prompt
  - WithResponseMIMEType: Set response format (e.g., "application/json")
  - WithResponseSchema: Define structured output schema
  - WithThinking: Enable thinking mode with a token budget
  - WithEffort: Enable thinking via the provider-neutral effort scale

# Thinking Mode

Thinking mode allows Gemini to show its internal reasoning process. When enabled,
thought blocks are captured in the trace. WithThinking sets a raw token budget,
which Gemini 2.x models take; Gemini 3.x models, including the default, take a
discrete thinking level, so use WithEffort with them:

	executor, err := googleexecutor.New[*Request, *Response](
	    client,
	    prompt,
	    googleexecutor.WithThinking[*Request, *Response](2048), // 2048 token budget for thinking
	)

WithEffort is the backend-portable alternative: it maps the shared effort.Level
scale onto the thinking control the configured model understands — a discrete
thinking level on Gemini 3.x and later, a token-budget tier on earlier models.
Configure exactly one of WithThinking and WithEffort:

	executor, err := googleexecutor.New[*Request, *Response](
	    client,
	    prompt,
	    googleexecutor.WithEffort[*Request, *Response](effort.High),
	)

Reasoning blocks are stored in trace.Reasoning as []agenttrace.ReasoningContent,
where each block contains:
  - Thinking: the reasoning text

# Gemini 4

Gemini 4 models take a thinking level and reject a thinking budget, so New
refuses WithThinking on them; use WithEffort. They also reject explicit context
caches, so the executor sends system instructions and tools inline and relies on
the provider's implicit prefix cache. A single request returns at most 32,768
output tokens (65,536 for a stream), but a generation can continue across
requests up to 1,048,576 tokens, input included: WithMaxOutputTokens accepts values up to that limit,
and the SDK follows the continuation tokens and merges the parts and usage
into one response. The extra room goes to reasoning; the model does
not lengthen its answers to fill it. Each continuation request re-sends the
prompt and is billed, and a retry after a failed continuation request starts
the generation again (see WithRetryConfig).

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
	    googleexecutor.WithModel[*Request, *Response]("gemini-3.8-flash"),
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
