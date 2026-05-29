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
const DefaultMaxTurns = 200

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
	boundPrompt, err := request.Bind(e.prompt)
	if err != nil {
		return response, fmt.Errorf("failed to bind request to prompt: %w", err)
	}

	prompt, err := boundPrompt.Build()
	if err != nil {
		return response, fmt.Errorf("failed to build prompt: %w", err)
	}

	trace, done := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() {
		done(response, err)
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
		kvs := []any{"tool", tc.Function.Name, "id", tc.ID}
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
			for k, v := range args {
				kvs = append(kvs, "args."+k, v)
			}
		}
		clog.InfoContext(ctx, "Executing tool call", kvs...)

		var res map[string]any
		if meta, ok := tools[tc.Function.Name]; ok {
			e.recordToolCall(ctx, tc.Function.Name)
			res = meta.Handler(ctx, tc, trace, finalResultPtr)
		} else {
			clog.ErrorContext(ctx, "Unknown tool requested", "tool", tc.Function.Name)
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

	// The named err return is load-bearing: the deferred Fail call below reads
	// it at function exit. Every error path must use `return ..., err` (or set
	// the named err before bare-returning) — a bare return inside a nested
	// block where err is shadowed via `:=` would silently bypass Fail.
	executeTurn := func(turn int) (_ Response, _ bool, err error) {
		llmTurn := trace.BeginTurn(turn, agenttrace.SystemOpenAI, e.modelName)
		defer func() {
			if err != nil {
				llmTurn.Fail(err)
			}
			llmTurn.End()
		}()

		// Per-turn retry config wires transient API errors that the retry
		// recovers from into the turn's Errors list. Without this, retries
		// that eventually succeed leave no trace of the transients in BQ.
		turnCfg := e.retryConfig
		turnCfg.OnAttemptError = llmTurn.RecordError

		// Capture the cumulative prompt as sent. reqParams.Messages grows
		// across turns (assistant + tool result messages are appended in
		// place), so each row carries the full context the model saw.
		// Gated inside on agenttrace.WithPayloadsEnabled.
		if err := llmTurn.RecordRequest(reqParams.Messages); err != nil {
			clog.WarnContext(ctx, "failed to record llm prompt payload", "error", err)
		}

		completion, err := retry.RetryWithBackoff(ctx, turnCfg, "chat_completion", isRetryableOpenAIError, func() (*openai.ChatCompletion, error) {
			return e.client.Chat.Completions.New(ctx, reqParams)
		})
		if err != nil {
			if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableOpenAIError, "OpenAI-compatible API"); requeueErr != nil {
				return response, true, requeueErr
			}
			return response, true, fmt.Errorf("failed to get completion (turn %d): %w", turn, err)
		}

		if completion.Usage.PromptTokens > 0 || completion.Usage.CompletionTokens > 0 {
			e.recordTokenMetrics(ctx, completion.Usage.PromptTokens, completion.Usage.CompletionTokens)
			llmTurn.RecordTokens(completion.Usage.PromptTokens, completion.Usage.CompletionTokens)
		}

		if len(completion.Choices) == 0 {
			return response, true, errors.New("no choices in completion response")
		}

		choice := completion.Choices[0]

		// Capture the assistant message (content + tool_calls + role) as the
		// completion for this turn. Pairs with RecordRequest above to produce
		// a per-span row keyed on prompt_hash. Gated inside the call.
		if err := llmTurn.RecordResponse(choice.Message); err != nil {
			clog.WarnContext(ctx, "failed to record llm response payload", "error", err)
		}

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
					return response, true, err
				}
				reqParams.Messages = append(reqParams.Messages, openai.ToolMessage(resJSON, tc.ID))
			}

			// Check if any tool set the final result.
			if !reflect.ValueOf(finalResult).IsZero() {
				clog.InfoContext(ctx, "Tool set final result, exiting conversation loop", "turns_completed", turn+1)
				e.recordTurns(ctx, turn+1, false)
				return finalResult, true, nil
			}
			return response, false, nil
		}

		textContent := choice.Message.Content

		// When submit_result is configured, redirect text responses back to the tool.
		if e.submitTool.Handler != nil && textContent != "" {
			clog.WarnContext(ctx, "Model responded with text instead of calling submit_result, redirecting")
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
			return response, false, nil
		}

		// Fallback: parse text response as JSON.
		if textContent != "" {
			resp, err := result.Extract[Response](textContent)
			if err != nil {
				clog.ErrorContext(ctx, "Failed to parse response",
					"response", textContent,
					"error", err)
				return response, true, fmt.Errorf("failed to parse response: %w", err)
			}
			clog.InfoContext(ctx, "Successfully completed OpenAI-compatible agent execution", "turns_completed", turn+1)
			e.recordTurns(ctx, turn+1, false)
			return resp, true, nil
		}

		return response, true, errors.New("no content in completion response")
	}

	for turn := range e.maxTurns {
		resp, done, err := executeTurn(turn)
		// done=true on all terminal paths (including errors); || err != nil is a
		// safety net in case a future path sets err without setting done.
		if done || err != nil {
			return resp, err
		}
	}

	clog.ErrorContext(ctx, "Agent exceeded maximum conversation turns", "max_turns", e.maxTurns)
	e.recordTurns(ctx, e.maxTurns, true)
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

// recordTurns records the number of turns used and, when limitExceeded is true,
// increments the turn_limit_exceeded counter.
func (e *executor[Request, Response]) recordTurns(ctx context.Context, turns int, limitExceeded bool) {
	attrs := e.resourceLabelsToAttributes()
	attrs = append(attrs, attribute.String("gen_ai.provider.name", "openai-compat"))
	e.genaiMetrics.RecordTurns(ctx, e.modelName, turns, limitExceeded, attrs...)
}
