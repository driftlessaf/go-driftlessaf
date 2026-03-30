/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

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

// DefaultMaxTurns is the default maximum number of conversation turns (LLM
// round-trips) before the executor aborts. Each turn corresponds to one
// Claude API call. This prevents runaway loops when the model keeps calling
// tools without converging on a result.
const DefaultMaxTurns = 50

// executor provides the private implementation
type executor[Request promptbuilder.Bindable, Response any] struct {
	client               anthropic.Client
	modelName            string
	systemInstructions   *promptbuilder.Prompt
	prompt               *promptbuilder.Prompt
	maxTokens            int64
	maxTurns             int // maximum conversation turns before aborting
	temperature          float64
	thinkingBudgetTokens *int64                        // nil = disabled, non-nil = enabled with budget
	submitTool           claudetool.Metadata[Response] // opt-in: set via WithSubmitResultProvider
	genaiMetrics         *metrics.GenAI                // OpenTelemetry metrics for token usage and tool calls
	retryConfig          retry.RetryConfig             // retry configuration for transient Claude API errors
	resourceLabels       map[string]string             // resource labels for GCP billing attribution

	// cacheControl enables Anthropic prompt caching. When true, the executor places
	// cache breakpoints on tool definitions and the system prompt so the API can skip
	// re-processing them on subsequent turns and executions. Cached tokens are read at
	// 10% of the base input token price (5-min TTL, shared across all requests with
	// the same prefix within the same org).
	// Enabled by default — disable with WithoutCacheControl() if needed.
	// See: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
	cacheControl bool
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
		maxTokens:    8192,            // Default max tokens
		maxTurns:     DefaultMaxTurns, // Default max conversation turns
		temperature:  0.1,             // Default temperature for consistency
		genaiMetrics: genaiMetrics,
		retryConfig:  retry.DefaultRetryConfig(), // Default retry config for rate limit handling
		cacheControl: true,                       // Prompt caching on by default — see cacheControl field comment
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

	clog.InfoContext(ctx, "Starting Claude agent execution",
		"prompt_length", len(prompt),
	)

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

	// Build tool definitions for Claude, sorted by name for deterministic ordering.
	//
	// Why sort? The Anthropic API uses prompt caching to avoid re-processing the
	// same content on every turn. It works by hashing the request prefix (tools →
	// system prompt → messages, in that order). If the hash matches a previous
	// request, cached tokens are served at 10% of the normal input token price.
	//
	// Go maps iterate in non-deterministic order, so without sorting, the tool
	// definitions would serialize differently on every turn — producing a different
	// hash and invalidating the cache every time, even though the tools haven't
	// changed. Sorting by name ensures a stable hash across turns and executions.
	toolDefs := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, meta := range tools {
		toolDefs = append(toolDefs, anthropic.ToolUnionParam{
			OfTool: &meta.Definition,
		})
	}
	slices.SortFunc(toolDefs, func(a, b anthropic.ToolUnionParam) int {
		return cmp.Compare(a.OfTool.Name, b.OfTool.Name)
	})

	// Place a cache breakpoint on the last tool definition.
	//
	// A "breakpoint" (cache_control) tells the API: "everything from the start of
	// the request up to and including this block can be cached." The API hashes
	// that prefix — if the next request has the same hash, the cached computation
	// is reused instead of re-processing all those tokens.
	//
	// Tools come first in the API prefix order (tools → system → messages), so
	// a breakpoint here caches all tool definitions. This benefits both multi-turn
	// conversations (same tools every turn) and separate executions that share the
	// same tool set (cache is keyed by content hash, not by session).
	if e.cacheControl && len(toolDefs) > 0 {
		toolDefs[len(toolDefs)-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
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
		systemBlock := anthropic.TextBlockParam{Text: systemPrompt}
		// Place a second cache breakpoint on the system prompt. Since the API prefix
		// order is tools → system → messages, this breakpoint caches both the tool
		// definitions AND the system prompt together. On subsequent turns, the API
		// reads both from cache instead of re-processing them as fresh input tokens.
		if e.cacheControl {
			systemBlock.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.System = []anthropic.TextBlockParam{systemBlock}
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
		l := log.With("tool", toolUse.Name).
			With("id", toolUse.ID)
		var args map[string]any
		if err := json.Unmarshal(toolUse.Input, &args); err == nil {
			for k, v := range args {
				l = l.With("args."+k, v)
			}
		}
		l.Info("Executing tool call")

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

	// Conversation loop with bounded turns to prevent runaway executions.
	for turn := 0; turn < e.maxTurns; turn++ {
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
			// If the error is a retryable Claude API error (429, 503, 504, 529) that
			// exhausted inner retries, signal the workqueue to back off instead of
			// immediately retrying — avoids contributing to API overload.
			if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableClaudeError, "Claude API"); requeueErr != nil {
				return response, requeueErr
			}
			return response, fmt.Errorf("failed to stream Claude response: %w", err)
		}

		// Record token usage in metrics and trace span
		if message.Usage.InputTokens > 0 || message.Usage.OutputTokens > 0 {
			e.recordTokenMetrics(ctx, message.Usage.InputTokens, message.Usage.OutputTokens)
			trace.RecordTokenUsage(e.modelName, message.Usage.InputTokens, message.Usage.OutputTokens)
		}

		// Record prompt cache metrics. The API response includes two cache-specific
		// token counts alongside the regular input/output tokens:
		//   - cache_read_input_tokens:     tokens served from cache (cheap, 0.1x price)
		//   - cache_creation_input_tokens: tokens written to cache (1.25x price, amortized over reads)
		// These are recorded as OTel counters and trace span attributes for cost analysis.
		if e.cacheControl {
			cacheRead := message.Usage.CacheReadInputTokens
			cacheCreation := message.Usage.CacheCreationInputTokens
			if cacheRead > 0 || cacheCreation > 0 {
				e.recordCacheMetrics(ctx, cacheRead, cacheCreation)
				trace.RecordCacheTokenUsage(cacheRead, cacheCreation)
				log.With("cache_read_tokens", cacheRead, "cache_creation_tokens", cacheCreation).
					Info("Prompt cache metrics")
			}
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

		// When submit_result is configured, it is the only valid exit path.
		// If Claude responds with text instead of calling submit_result,
		// force it to call the tool on the next turn using tool_choice.
		if e.submitTool.Handler != nil && textContent != "" {
			log.Warn("Claude responded with text instead of calling submit_result, redirecting with tool_choice")
			e.recordToolCall(ctx, "submit_result_redirect")

			submitToolName := e.submitTool.Definition.Name
			if submitToolName == "" {
				submitToolName = "submit_result"
			}

			params.Messages = append(params.Messages, message.ToParam())
			params.Messages = append(params.Messages, anthropic.MessageParam{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					anthropic.NewTextBlock("You must call the submit_result tool to return your response. Do not respond with plain text. If you encountered an error or cannot complete the task, call submit_result with an appropriate error or summary."),
				},
			})
			// Force Claude to call the submit_result tool on the next turn.
			params.ToolChoice = anthropic.ToolChoiceParamOfTool(submitToolName)
			continue
		}

		// Fallback: parse text response as JSON when submit_result is not configured
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

	log.With("max_turns", e.maxTurns).Error("Agent exceeded maximum conversation turns")
	return response, fmt.Errorf("agent exceeded maximum conversation turns (%d)", e.maxTurns)
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
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordTokens(ctx, e.modelName, inputTokens, outputTokens, attrs...)
}

// recordCacheMetrics records prompt cache token usage with optional enrichment
func (e *executor[Request, Response]) recordCacheMetrics(ctx context.Context, cacheRead, cacheCreation int64) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordCacheTokens(ctx, e.modelName, cacheRead, cacheCreation, attrs...)
}

// recordToolCall records a tool call metric with optional enrichment
func (e *executor[Request, Response]) recordToolCall(ctx context.Context, toolName string) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "anthropic"))
	e.genaiMetrics.RecordToolCall(ctx, e.modelName, toolName, attrs...)
}
