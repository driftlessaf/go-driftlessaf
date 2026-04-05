/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

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
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"go.opentelemetry.io/otel/attribute"
)

// Interface is the public interface for OpenAI-compatible agent execution.
type Interface[Request promptbuilder.Bindable, Response any] interface {
	// Execute runs the agent conversation with the given request and tools.
	Execute(ctx context.Context, request Request, tools map[string]openaistool.Metadata[Response]) (Response, error)
}

// DefaultMaxTurns is the default maximum number of conversation turns before aborting.
const DefaultMaxTurns = 50

type executor[Request promptbuilder.Bindable, Response any] struct {
	client             openai.Client
	modelName          string
	systemInstructions *promptbuilder.Prompt
	prompt             *promptbuilder.Prompt
	maxTokens          int64
	maxTurns           int
	temperature        float64
	submitTool         openaistool.Metadata[Response]
	genaiMetrics       *metrics.GenAI
	retryConfig        retry.RetryConfig
	resourceLabels     map[string]string
}

// New creates a new OpenAI-compatible executor.
func New[Request promptbuilder.Bindable, Response any](
	client openai.Client,
	prompt *promptbuilder.Prompt,
	opts ...Option[Request, Response],
) (Interface[Request, Response], error) {
	if prompt == nil {
		return nil, errors.New("prompt cannot be nil")
	}

	e := &executor[Request, Response]{
		client:       client,
		modelName:    "google/gemini-2.5-flash",
		prompt:       prompt,
		maxTokens:    8192,
		maxTurns:     DefaultMaxTurns,
		temperature:  0.1,
		genaiMetrics: metrics.NewGenAI("chainguard.ai.agents"),
		retryConfig:  retry.DefaultRetryConfig(),
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	return e, nil
}

// Execute runs the agent conversation with the given request and tools.
func (e *executor[Request, Response]) Execute(
	ctx context.Context,
	request Request,
	tools map[string]openaistool.Metadata[Response],
) (response Response, err error) {
	log := clog.FromContext(ctx)

	boundPrompt, err := request.Bind(e.prompt)
	if err != nil {
		return response, fmt.Errorf("failed to bind request to prompt: %w", err)
	}

	prompt, err := boundPrompt.Build()
	if err != nil {
		return response, fmt.Errorf("failed to build prompt: %w", err)
	}

	trace := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() {
		trace.Complete(response, err)
	}()

	clog.InfoContext(ctx, "Starting OpenAI-compatible agent execution",
		"model", e.modelName,
		"prompt_length", len(prompt),
	)

	// Merge submit_result tool if configured.
	if e.submitTool.Handler != nil {
		mergedTools := make(map[string]openaistool.Metadata[Response], len(tools)+1)
		maps.Copy(mergedTools, tools)
		name := e.submitTool.Definition.Function.Name
		if name == "" {
			name = "submit_result"
		}
		if _, exists := mergedTools[name]; !exists {
			mergedTools[name] = e.submitTool
		}
		tools = mergedTools
	}

	// Build tool definitions.
	toolDefs := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, meta := range tools {
		toolDefs = append(toolDefs, meta.Definition)
	}

	// Build initial messages.
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(prompt),
	}

	if e.systemInstructions != nil {
		systemPrompt, err := e.systemInstructions.Build()
		if err != nil {
			return response, fmt.Errorf("building system prompt: %w", err)
		}
		messages = append([]openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
		}, messages...)
	}

	reqParams := openai.ChatCompletionNewParams{
		Model:               e.modelName,
		Messages:            messages,
		Tools:               toolDefs,
		MaxCompletionTokens: param.NewOpt(e.maxTokens),
		Temperature:         param.NewOpt(e.temperature),
	}

	var finalResult Response
	finalResultPtr := &finalResult

	executeToolCall := func(tc openai.ChatCompletionMessageToolCall) (string, error) {
		l := log.With("tool", tc.Function.Name).With("id", tc.ID)
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
			for k, v := range args {
				l = l.With("args."+k, v)
			}
		}
		l.Info("Executing tool call")

		var res map[string]any
		if meta, ok := tools[tc.Function.Name]; ok {
			e.recordToolCall(ctx, tc.Function.Name)
			res = meta.Handler(ctx, tc, trace, finalResultPtr)
		} else {
			log.With("tool", tc.Function.Name).Error("Unknown tool requested")
			trace.BadToolCall(tc.ID, tc.Function.Name,
				map[string]any{"arguments": tc.Function.Arguments},
				fmt.Errorf("unknown tool: %q", tc.Function.Name))
			res = map[string]any{"error": fmt.Sprintf("unknown tool: %q", tc.Function.Name)}
		}

		resBytes, err := json.Marshal(res)
		if err != nil {
			return "", fmt.Errorf("failed to marshal tool result: %w", err)
		}
		return string(resBytes), nil
	}

	for turn := range e.maxTurns {
		completion, err := retry.RetryWithBackoff(ctx, e.retryConfig, "chat_completion", isRetryableOpenAIError, func() (*openai.ChatCompletion, error) {
			return e.client.Chat.Completions.New(ctx, reqParams)
		})
		if err != nil {
			if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableOpenAIError, "OpenAI-compatible API"); requeueErr != nil {
				return response, requeueErr
			}
			return response, fmt.Errorf("failed to get completion (turn %d): %w", turn, err)
		}

		if completion.Usage.PromptTokens > 0 || completion.Usage.CompletionTokens > 0 {
			e.recordTokenMetrics(ctx, completion.Usage.PromptTokens, completion.Usage.CompletionTokens)
			trace.RecordTokenUsage(e.modelName, completion.Usage.PromptTokens, completion.Usage.CompletionTokens)
		}

		if len(completion.Choices) == 0 {
			return response, errors.New("no choices in completion response")
		}

		choice := completion.Choices[0]

		// Capture reasoning_content from thinking models (e.g. kimi-k2-thinking-maas).
		// This field is non-standard and arrives via ExtraFields.
		if f, ok := choice.Message.JSON.ExtraFields["reasoning_content"]; ok {
			var thinking string
			if json.Unmarshal([]byte(f.Raw()), &thinking) == nil && thinking != "" {
				trace.Reasoning = append(trace.Reasoning, agenttrace.ReasoningContent{
					Thinking: thinking,
				})
			}
		}

		// Handle tool calls.
		if len(choice.Message.ToolCalls) > 0 {
			// Add assistant message with tool calls to conversation.
			reqParams.Messages = append(reqParams.Messages, choice.Message.ToParam())

			// Execute all tool calls and collect results before checking for a final
			// result. This ensures the conversation history is always consistent —
			// all tool result messages are appended even if submit_result fires midway.
			for _, tc := range choice.Message.ToolCalls {
				resJSON, err := executeToolCall(tc)
				if err != nil {
					return response, err
				}
				reqParams.Messages = append(reqParams.Messages, openai.ToolMessage(resJSON, tc.ID))
			}

			// Check if any tool set the final result.
			if !reflect.ValueOf(finalResult).IsZero() {
				log.Info("Tool set final result, exiting conversation loop")
				return finalResult, nil
			}
			continue
		}

		textContent := choice.Message.Content

		// When submit_result is configured, redirect text responses back to the tool.
		if e.submitTool.Handler != nil && textContent != "" {
			log.Warn("Model responded with text instead of calling submit_result, redirecting")
			e.recordToolCall(ctx, "submit_result_redirect")

			submitToolName := e.submitTool.Definition.Function.Name
			if submitToolName == "" {
				submitToolName = "submit_result"
			}

			reqParams.Messages = append(reqParams.Messages, choice.Message.ToParam())
			reqParams.Messages = append(reqParams.Messages,
				openai.UserMessage(fmt.Sprintf("You must call the %s tool to return your response. Do not respond with plain text. If you encountered an error or cannot complete the task, call %s with an appropriate error or summary.", submitToolName, submitToolName)),
			)
			// Note: we intentionally do not set tool_choice here — some models (e.g. reasoning
			// models) do not support named tool_choice and return 400. The user message alone
			// is sufficient to redirect the model to call the right tool.
			continue
		}

		// Fallback: parse text response as JSON.
		if textContent != "" {
			resp, err := result.Extract[Response](textContent)
			if err != nil {
				log.With("response", textContent).With("error", err).Error("Failed to parse response")
				return response, fmt.Errorf("failed to parse response: %w", err)
			}
			log.Info("Successfully completed OpenAI-compatible agent execution")
			return resp, nil
		}

		return response, errors.New("no content in completion response")
	}

	log.With("max_turns", e.maxTurns).Error("Agent exceeded maximum conversation turns")
	return response, fmt.Errorf("agent exceeded maximum conversation turns (%d)", e.maxTurns)
}

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

func (e *executor[Request, Response]) recordTokenMetrics(ctx context.Context, inputTokens, outputTokens int64) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "openai-compat"))
	e.genaiMetrics.RecordTokens(ctx, e.modelName, inputTokens, outputTokens, attrs...)
}

func (e *executor[Request, Response]) recordToolCall(ctx context.Context, toolName string) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "openai-compat"))
	e.genaiMetrics.RecordToolCall(ctx, e.modelName, toolName, attrs...)
}
