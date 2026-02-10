/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/result"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"go.opentelemetry.io/otel/attribute"
)

// Interface is the public interface for Claude agent execution
type Interface[Request promptbuilder.Bindable, Response any] interface {
	// Execute runs the agent conversation with the given request and tools
	// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
	Execute(ctx context.Context, request Request, tools map[string]claudetool.Metadata[Response], seedToolCalls ...anthropic.ToolUseBlock) (Response, error)
}

// executor provides the private implementation
type executor[Request promptbuilder.Bindable, Response any] struct {
	client               anthropic.Client
	modelName            string
	systemInstructions   *promptbuilder.Prompt
	prompt               *promptbuilder.Prompt
	maxTokens            int64
	temperature          float64
	thinkingBudgetTokens *int64                        // nil = disabled, non-nil = enabled with budget
	submitTool           claudetool.Metadata[Response] // opt-in: set via WithSubmitResultProvider
	genaiMetrics         *metrics.GenAI                // OpenTelemetry metrics for token usage and tool calls
	retryConfig          retry.RetryConfig             // retry configuration for transient Claude API errors
	resourceLabels       map[string]string             // resource labels for GCP billing attribution
}

// New creates a new Executor with minimal required configuration
func New[Request promptbuilder.Bindable, Response any](
	client anthropic.Client,
	prompt *promptbuilder.Prompt,
	opts ...Option[Request, Response],
) (Interface[Request, Response], error) {
	// Validate inputs
	if prompt == nil {
		return nil, errors.New("prompt cannot be nil")
	}

	// Create GenAI metrics for observability
	// Uses a unified meter across all executors, with model name as a dimension
	genaiMetrics := metrics.NewGenAI("chainguard.ai.agents")

	e := &executor[Request, Response]{
		client:       client,
		modelName:    "claude-sonnet-4@20250514", // Default to Sonnet 4
		prompt:       prompt,
		maxTokens:    8192, // Default max tokens
		temperature:  0.1,  // Default temperature for consistency
		genaiMetrics: genaiMetrics,
		retryConfig:  retry.DefaultRetryConfig(), // Default retry config for rate limit handling
	}

	// Apply options
	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	return e, nil
}

// Execute runs the agent conversation with the given request and tools
// Optional seed tool calls can be provided - these will be executed and their results prepended to the conversation
func (e *executor[Request, Response]) Execute(
	ctx context.Context,
	request Request,
	tools map[string]claudetool.Metadata[Response],
	seedToolCalls ...anthropic.ToolUseBlock,
) (response Response, err error) {
	log := clog.FromContext(ctx)

	// Bind the request to the prompt
	boundPrompt, err := request.Bind(e.prompt)
	if err != nil {
		return response, fmt.Errorf("failed to bind request to prompt: %w", err)
	}

	// Build the prompt string
	prompt, err := boundPrompt.Build()
	if err != nil {
		return response, fmt.Errorf("failed to build prompt: %w", err)
	}

	// Start trace
	trace := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() {
		trace.Complete(response, err)
	}()

	log.With("prompt_length", len(prompt)).
		Info("Starting Claude agent execution")

	// Merge submit_result tool if configured (opt-in via WithSubmitResultProvider)
	if e.submitTool.Handler != nil {
		mergedTools := make(map[string]claudetool.Metadata[Response], len(tools)+1)
		maps.Copy(mergedTools, tools)

		name := e.submitTool.Definition.Name
		if name == "" {
			name = "submit_result"
		}
		if _, exists := mergedTools[name]; !exists {
			mergedTools[name] = e.submitTool
		}
		tools = mergedTools
	}

	// Build tool definitions for Claude
	toolDefs := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, meta := range tools {
		toolDefs = append(toolDefs, anthropic.ToolUnionParam{
			OfTool: &meta.Definition,
		})
	}

	// Create initial messages, starting with the user prompt
	messages := []anthropic.MessageParam{{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewTextBlock(prompt),
		},
	}}

	// Create request parameters
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(e.modelName),
		MaxTokens: e.maxTokens,
		Messages:  messages,
		Tools:     toolDefs,
	}

	params.Temperature = anthropic.Float(e.temperature)
	// Set temperature - must be 1.0 when thinking is enabled
	// See: https://docs.claude.com/en/docs/build-with-claude/extended-thinking#important-considerations-when-using-extended-thinking
	if e.thinkingBudgetTokens != nil {
		params.Temperature = anthropic.Float(1.0)
	}

	// Add system instructions if provided
	if e.systemInstructions != nil {
		systemPrompt, err := e.systemInstructions.Build()
		if err != nil {
			return response, fmt.Errorf("building system prompt: %w", err)
		}
		params.System = []anthropic.TextBlockParam{{Text: systemPrompt}}
	}

	// Add thinking configuration if enabled
	if e.thinkingBudgetTokens != nil {
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{
				BudgetTokens: *e.thinkingBudgetTokens,
			},
		}
	}

	// finalResult stores the result if a tool sets it
	var finalResult Response
	finalResultPtr := &finalResult

	// executeToolCall handles executing a single tool call and returning the result
	executeToolCall := func(toolUse anthropic.ToolUseBlock) (anthropic.ContentBlockParamUnion, error) {
		log.With("tool", toolUse.Name).
			With("id", toolUse.ID).
			Info("Executing tool call")

		var result map[string]any

		if meta, ok := tools[toolUse.Name]; ok {
			// Execute registered handler with result pointer
			result = meta.Handler(ctx, toolUse, trace, finalResultPtr)
		} else {
			// Unknown tool
			log.With("tool", toolUse.Name).Error("Unknown tool requested")
			trace.BadToolCall(toolUse.ID, toolUse.Name,
				map[string]any{"input": toolUse.Input},
				fmt.Errorf("unknown tool: %q", toolUse.Name))

			result = map[string]any{
				"error": fmt.Sprintf("unknown tool: %q", toolUse.Name),
			}
		}

		// Marshal result
		resultBytes, err := json.Marshal(result)
		if err != nil {
			return anthropic.ContentBlockParamUnion{}, fmt.Errorf("failed to marshal tool result: %w", err)
		}

		return anthropic.ContentBlockParamUnion{
			OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: toolUse.ID,
				Content: []anthropic.ToolResultBlockParamContentUnion{{
					OfText: &anthropic.TextBlockParam{
						Text: string(resultBytes),
					},
				}},
			},
		}, nil
	}

	// Pre-execute seed tool calls and add them to messages
	for _, toolCall := range seedToolCalls {
		// Add assistant message with this tool call
		params.Messages = append(params.Messages, anthropic.MessageParam{
			Role: anthropic.MessageParamRoleAssistant,
			Content: []anthropic.ContentBlockParamUnion{{
				OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    toolCall.ID,
					Name:  toolCall.Name,
					Input: toolCall.Input,
				},
			}},
		})

		// Execute the tool call
		result, err := executeToolCall(toolCall)
		if err != nil {
			return response, err
		}

		// Check if a tool set the final result during seed execution
		if !reflect.ValueOf(finalResult).IsZero() {
			log.Info("Seed tool set final result, exiting immediately")
			return finalResult, nil
		}

		// Add tool result to conversation
		params.Messages = append(params.Messages, anthropic.MessageParam{
			Role:    anthropic.MessageParamRoleUser,
			Content: []anthropic.ContentBlockParamUnion{result},
		})
	}

	// Conversation loop
	for {
		// Stream response with retry for transient errors
		message, err := retry.RetryWithBackoff(ctx, e.retryConfig, "stream_message", isRetryableClaudeError, func() (anthropic.Message, error) {
			stream := e.client.Messages.NewStreaming(ctx, params)
			var msg anthropic.Message
			for stream.Next() {
				event := stream.Current()
				if err := msg.Accumulate(event); err != nil {
					return msg, fmt.Errorf("failed to accumulate event: %w", err)
				}
			}
			if err := stream.Err(); err != nil {
				return msg, err
			}
			return msg, nil
		})
		if err != nil {
			return response, fmt.Errorf("failed to stream Claude response: %w", err)
		}

		// Record token usage in metrics and trace span
		if message.Usage.InputTokens > 0 || message.Usage.OutputTokens > 0 {
			e.recordTokenMetrics(ctx, message.Usage.InputTokens, message.Usage.OutputTokens)
			// Also record on trace span for easy viewing in Cloud Trace
			trace.RecordTokenUsage(e.modelName, message.Usage.InputTokens, message.Usage.OutputTokens)
		}

		// Process response
		var toolUseBlocks []anthropic.ToolUseBlock
		var textContent string

		for _, content := range message.Content {
			switch content.Type {
			case "text":
				textContent = content.Text
			case "tool_use":
				toolUseBlocks = append(toolUseBlocks, anthropic.ToolUseBlock{
					ID:    content.ID,
					Name:  content.Name,
					Input: content.Input,
				})
			case "thinking", "redacted_thinking":
				trace.Reasoning = append(trace.Reasoning, agenttrace.ReasoningContent{
					Thinking: content.Thinking,
				})
			}
		}

		// Handle tool calls
		if len(toolUseBlocks) > 0 {
			// Add Claude's response to conversation
			params.Messages = append(params.Messages, message.ToParam())

			// Execute the tool calls
			var toolResults []anthropic.ContentBlockParamUnion
			for _, toolUse := range toolUseBlocks {
				// Record tool call metric
				e.recordToolCall(ctx, toolUse.Name)

				result, err := executeToolCall(toolUse)
				if err != nil {
					return response, err
				}
				toolResults = append(toolResults, result)

				// Check if a tool set the final result
				if !reflect.ValueOf(finalResult).IsZero() {
					log.Info("Tool set final result, exiting conversation loop")
					return finalResult, nil
				}
			}

			// Add tool results to conversation
			params.Messages = append(params.Messages, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: toolResults,
			})

			continue
		}

		// Parse final response
		if textContent != "" {
			resp, err := result.Extract[Response](textContent)
			if err != nil {
				log.With("response", textContent).
					With("error", err).
					Error("Failed to parse Claude response")
				return response, fmt.Errorf("failed to parse response: %w", err)
			}

			log.Info("Successfully completed Claude agent execution")
			return resp, nil
		}

		return response, errors.New("no content in Claude's response")
	}
}

// resourceLabelsToAttributes converts resourceLabels map to OpenTelemetry attributes
func (e *executor[Request, Response]) resourceLabelsToAttributes() []attribute.KeyValue {
	if len(e.resourceLabels) == 0 {
		return nil
	}
	attrs := make([]attribute.KeyValue, 0, len(e.resourceLabels))
	for k, v := range e.resourceLabels {
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}

// recordTokenMetrics records token usage with optional enrichment
func (e *executor[Request, Response]) recordTokenMetrics(ctx context.Context, inputTokens, outputTokens int64) {
	attrs := e.resourceLabelsToAttributes()
	e.genaiMetrics.RecordTokens(ctx, e.modelName, inputTokens, outputTokens, attrs...)
}

// recordToolCall records a tool call metric with optional enrichment
func (e *executor[Request, Response]) recordToolCall(ctx context.Context, toolName string) {
	attrs := e.resourceLabelsToAttributes()
	e.genaiMetrics.RecordToolCall(ctx, e.modelName, toolName, attrs...)
}
