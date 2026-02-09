/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/result"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"
)

// Interface defines the contract for Google AI executors
type Interface[Request promptbuilder.Bindable, Response any] interface {
	// Execute runs the Google AI conversation with the given request and tools
	// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
	Execute(ctx context.Context, request Request, tools map[string]googletool.Metadata[Response], seedToolCalls ...*genai.FunctionCall) (Response, error)
}

// executor is the private implementation of Interface
type executor[Request promptbuilder.Bindable, Response any] struct {
	client             *genai.Client
	prompt             *promptbuilder.Prompt
	model              string
	temperature        float32
	maxOutputTokens    int32
	systemInstructions *promptbuilder.Prompt
	responseMIMEType   string
	responseSchema     *genai.Schema
	thinkingBudget     *int32                        // nil = disabled, non-nil = enabled with budget
	submitTool         googletool.Metadata[Response] // opt-in: set via WithSubmitResultProvider
	genaiMetrics       *metrics.GenAI                // OpenTelemetry metrics for token usage and tool calls
	retryConfig        retry.RetryConfig             // retry configuration for transient Vertex AI errors
}

// New creates a new Google AI executor with the given configuration
func New[Request promptbuilder.Bindable, Response any](
	client *genai.Client,
	prompt *promptbuilder.Prompt,
	options ...Option[Request, Response],
) (Interface[Request, Response], error) {
	if prompt == nil {
		return nil, errors.New("prompt is required")
	}

	// Create GenAI metrics for observability
	// Uses a unified meter across all executors, with model name as a dimension
	genaiMetrics := metrics.NewGenAI("chainguard.ai.agents")

	// Create executor with defaults
	exec := &executor[Request, Response]{
		client:          client,
		prompt:          prompt,
		model:           "gemini-2.5-flash", // Default to Gemini 2.5 Flash
		temperature:     0.1,                // Default temperature for consistency
		maxOutputTokens: 8192,               // Default max tokens
		genaiMetrics:    genaiMetrics,
		retryConfig:     retry.DefaultRetryConfig(), // Default retry config for rate limit handling
	}

	// Apply options
	for _, opt := range options {
		if err := opt(exec); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	return exec, nil
}

// Execute implements the Interface
// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
func (e *executor[Request, Response]) Execute(
	ctx context.Context,
	request Request,
	tools map[string]googletool.Metadata[Response],
	seedToolCalls ...*genai.FunctionCall,
) (resp Response, err error) {
	log := clog.FromContext(ctx)

	// Bind the request to the prompt
	boundPrompt, err := request.Bind(e.prompt)
	if err != nil {
		return resp, fmt.Errorf("failed to bind request to prompt: %w", err)
	}

	// Build the prompt string
	prompt, err := boundPrompt.Build()
	if err != nil {
		return resp, fmt.Errorf("failed to build prompt: %w", err)
	}

	// Start a trace for this execution
	trace := evals.StartTrace[Response](ctx, prompt)
	defer func() {
		trace.Complete(resp, err)
	}()

	// Merge submit_result tool if configured (opt-in via WithSubmitResultProvider)
	if e.submitTool.Handler != nil {
		mergedTools := make(map[string]googletool.Metadata[Response], len(tools)+1)
		maps.Copy(mergedTools, tools)

		name := "submit_result"
		if e.submitTool.Definition != nil && e.submitTool.Definition.Name != "" {
			name = e.submitTool.Definition.Name
		}
		if _, exists := mergedTools[name]; !exists {
			mergedTools[name] = e.submitTool
		}
		tools = mergedTools
	}

	toolDeclarations := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, meta := range tools {
		toolDeclarations = append(toolDeclarations, meta.Definition)
	}

	// Create generation config
	config := &genai.GenerateContentConfig{
		Temperature:     ptr(e.temperature),
		MaxOutputTokens: e.maxOutputTokens,
	}

	// Add system instructions if provided
	if e.systemInstructions != nil {
		systemPrompt, err := e.systemInstructions.Build()
		if err != nil {
			return resp, fmt.Errorf("building system prompt: %w", err)
		}
		config.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{
				Text: systemPrompt,
			}},
		}
	}

	// Add tools if provided
	if len(toolDeclarations) > 0 {
		config.Tools = []*genai.Tool{{
			FunctionDeclarations: toolDeclarations,
		}}
	}

	// Add response MIME type if provided
	if e.responseMIMEType != "" {
		config.ResponseMIMEType = e.responseMIMEType
	}

	// Add response schema if provided
	if e.responseSchema != nil {
		config.ResponseSchema = e.responseSchema
	}

	// Add thinking configuration if enabled
	if e.thinkingBudget != nil {
		config.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingBudget:  e.thinkingBudget,
		}
	}

	// Create a new chat session with optional seed messages
	log.With("model", e.model).Info("Creating Google AI chat session")

	// Pre-execute seed tool calls and prepare history
	// Build complete history, then split: use first n-1 for chat creation, send last via SendMessage
	history := make([]*genai.Content, 0, 1+len(seedToolCalls)*2)

	// Add initial user prompt to history
	history = append(history, &genai.Content{
		Role: "user",
		Parts: []*genai.Part{{
			Text: prompt,
		}},
	})

	// finalResult stores the result if a tool sets it
	var finalResult Response
	finalResultPtr := &finalResult

	// Execute seed tool calls and build complete history
	for _, call := range seedToolCalls {
		log.With("tool", call.Name).With("id", call.ID).Info("Pre-executing seed tool call")

		// Execute the tool call
		var result *genai.FunctionResponse
		if meta, ok := tools[call.Name]; ok {
			result = meta.Handler(ctx, call, trace, finalResultPtr)
		} else {
			log.With("tool", call.Name).Error("Unknown seed tool requested")
			trace.BadToolCall(call.ID, call.Name, call.Args, fmt.Errorf("unknown tool: %q", call.Name))
			result = &genai.FunctionResponse{
				ID:   call.ID,
				Name: call.Name,
				Response: map[string]any{
					"error": fmt.Sprintf("unknown tool: %q", call.Name),
				},
			}
		}

		// Check if a tool set the final result during seed execution
		if !reflect.ValueOf(finalResult).IsZero() {
			log.Info("Seed tool set final result, exiting immediately")
			return finalResult, nil
		}

		// Add model response with function call and function response
		history = append(history, &genai.Content{
			Role: "model",
			Parts: []*genai.Part{{
				FunctionCall: call,
			}},
		}, &genai.Content{
			Role: "user",
			Parts: []*genai.Part{{
				FunctionResponse: result,
			}},
		})
	}

	// Create chat with first n-1 messages, send last message separately
	chat, err := e.client.Chats.Create(ctx, e.model, config, history[:len(history)-1])
	if err != nil {
		return resp, fmt.Errorf("failed to create chat with model %q: %w", e.model, err)
	}

	// Send final message to get response with retry for transient errors
	log.Info("Sending final message")
	response, err := retry.RetryWithBackoff(ctx, e.retryConfig, "send_initial_message", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
		return chat.Send(ctx, history[len(history)-1].Parts...)
	})
	if err != nil {
		return resp, fmt.Errorf("failed to send final message: %w", err)
	}

	if response != nil && response.UsageMetadata != nil {
		e.recordTokenMetrics(ctx, response.UsageMetadata)
		// Also record on trace span for easy viewing in Cloud Trace
		trace.RecordTokenUsage(e.model, int64(response.UsageMetadata.PromptTokenCount), int64(response.UsageMetadata.CandidatesTokenCount))
	}

	// Handle the conversation loop
	var responseText string
	for {
		log.With("candidates_count", len(response.Candidates)).
			Info("Received response from model")

		if len(response.Candidates) == 0 {
			return resp, errors.New("no content generated - no candidates")
		}

		candidate := response.Candidates[0]

		// Check for malformed function call
		if candidate.FinishReason == genai.FinishReasonMalformedFunctionCall {
			log.With("finish_message", candidate.FinishMessage).
				Warn("Model attempted a malformed function call, asking it to retry")

			// Build available function names for retry message
			var funcNames []string
			for _, decl := range toolDeclarations {
				funcNames = append(funcNames, decl.Name)
			}

			// Send a message asking the model to try again with retry for transient errors
			retryMsg := genai.Part{Text: fmt.Sprintf("The function call was malformed. Please try again using the available functions: %v", funcNames)}
			retryResp, err := retry.RetryWithBackoff(ctx, e.retryConfig, "send_malformed_retry", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
				return chat.SendMessage(ctx, retryMsg)
			})
			if err != nil {
				return resp, fmt.Errorf("failed to send retry message after malformed function call: %w", err)
			}

			// Record metrics for retry call
			if retryResp != nil && retryResp.UsageMetadata != nil {
				e.recordTokenMetrics(ctx, retryResp.UsageMetadata)
				// Also record on trace span for easy viewing in Cloud Trace
				trace.RecordTokenUsage(e.model, int64(retryResp.UsageMetadata.PromptTokenCount), int64(retryResp.UsageMetadata.CandidatesTokenCount))
			}

			// Continue with the new response
			response = retryResp
			continue
		}

		if candidate.Content == nil {
			return resp, errors.New("no content generated - candidate content is nil")
		}

		if len(candidate.Content.Parts) == 0 {
			return resp, errors.New("no content generated - no parts in candidate")
		}

		// Check for function calls or text
		var toolCalls []*genai.FunctionCall
		var hasText bool

		for i, part := range candidate.Content.Parts {
			switch {
			case part.Thought:
				trace.Reasoning = append(trace.Reasoning, evals.ReasoningContent{
					Thinking: part.Text,
				})
				log.With("part_index", i).
					With("thinking_length", len(part.Text)).
					Info("Found thought part")
			case part.Text != "":
				responseText = part.Text
				hasText = true
				log.With("part_index", i).
					With("text_length", len(part.Text)).
					Info("Found text part")
			case part.FunctionCall != nil:
				toolCalls = append(toolCalls, part.FunctionCall)
				log.With("part_index", i).
					With("function_name", part.FunctionCall.Name).
					With("function_id", part.FunctionCall.ID).
					Info("Found function call part")
			default:
				log.With("part_index", i).
					Warn("Found part with unexpected content")
			}
		}

		// If there are tool calls, execute them and send responses
		if len(toolCalls) > 0 {
			var toolResponseParts []*genai.Part

			for _, call := range toolCalls {
				log.With("tool", call.Name).With("id", call.ID).Info("Executing tool call")

				// Record tool call metric
				e.recordToolCall(ctx, call.Name)

				// Find and execute the handler for this tool
				var toolResponse *genai.FunctionResponse
				toolMeta, found := tools[call.Name]
				if !found {
					log.With("function", call.Name).Error("Unknown function call requested by model")
					toolResponse = googletool.Error(call, "Unknown function: %s", call.Name)

					// Record bad tool call for unknown function
					trace.BadToolCall(call.ID, call.Name, call.Args, fmt.Errorf("unknown function: %q", call.Name))
				} else {
					// Execute the tool handler
					toolResponse = toolMeta.Handler(ctx, call, trace, finalResultPtr)
				}

				// Check if a tool set the final result
				if !reflect.ValueOf(finalResult).IsZero() {
					log.Info("Tool set final result, exiting conversation loop")
					return finalResult, nil
				}

				toolResponseParts = append(toolResponseParts, &genai.Part{
					FunctionResponse: toolResponse,
				})
			}

			// Send tool responses back to the chat with retry for transient errors
			response, err = retry.RetryWithBackoff(ctx, e.retryConfig, "send_tool_responses", isRetryableVertexError, func() (*genai.GenerateContentResponse, error) {
				return chat.Send(ctx, toolResponseParts...)
			})
			if err != nil {
				return resp, fmt.Errorf("failed to send tool responses: %w", err)
			}

			if response != nil && response.UsageMetadata != nil {
				e.recordTokenMetrics(ctx, response.UsageMetadata)
				// Also record on trace span for easy viewing in Cloud Trace
				trace.RecordTokenUsage(e.model, int64(response.UsageMetadata.PromptTokenCount), int64(response.UsageMetadata.CandidatesTokenCount))
			}
			continue
		}

		// If we have text, we're done
		if hasText && responseText != "" {
			break
		}

		// Unexpected state
		log.Error("Unexpected response format - no text and no tool calls")
		return resp, errors.New("unexpected response format from model")
	}

	if responseText == "" {
		return resp, errors.New("no text content found in response")
	}

	// Extract and parse the response
	extractedResponse, err := result.Extract[Response](responseText)
	if err != nil {
		log.With("response", responseText).With("error", err).Error("Failed to parse AI response")
		return resp, fmt.Errorf("failed to parse AI response: %w", err)
	}

	return extractedResponse, nil
}

// ptr is a helper function to create a pointer to a value
func ptr[T any](v T) *T {
	return &v
}

// recordTokenMetrics records token usage with optional enrichment
func (e *executor[Request, Response]) recordTokenMetrics(ctx context.Context, usage *genai.GenerateContentResponseUsageMetadata) {
	if usage == nil {
		return
	}

	e.genaiMetrics.RecordTokens(ctx, e.model, int64(usage.PromptTokenCount), int64(usage.CandidatesTokenCount))
}

// recordToolCall records a tool call metric with optional enrichment
func (e *executor[Request, Response]) recordToolCall(ctx context.Context, toolName string) {
	e.genaiMetrics.RecordToolCall(ctx, e.model, toolName)
}
