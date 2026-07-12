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
	"reflect"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/result"
	"chainguard.dev/driftlessaf/agents/schema"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"
)

// Interface is the public interface for OpenAI-compatible agent execution.
type Interface[Request promptbuilder.Bindable, Response any] interface {
	// Execute runs the agent conversation with the given request and tools.
	Execute(ctx context.Context, request Request, tools map[string]openaistool.Metadata[Response]) (Response, error)
}

// DefaultMaxTurns is the default maximum number of conversation turns before aborting.
const DefaultMaxTurns = 200

// DefaultToolCallConcurrency is the default bound on how many of a single
// turn's tool calls run concurrently. Models routinely emit several
// independent tool calls in one turn (parallel tool calls); dispatching their
// handlers concurrently cuts wall-clock latency. Override with
// WithToolCallConcurrency — a value of 1 restores strictly sequential
// dispatch.
const DefaultToolCallConcurrency = 10

type executor[Request promptbuilder.Bindable, Response any] struct {
	client             openai.Client
	modelName          string
	systemInstructions *promptbuilder.Prompt
	prompt             *promptbuilder.Prompt

	// userPromptSuffix, when non-nil, is a static operator-authored prompt
	// appended to the built user prompt with a blank-line separator. See
	// WithUserPromptSuffix; the request is never bound into it.
	userPromptSuffix *promptbuilder.Prompt

	maxTokens      int64
	maxTurns       int
	temperature    float64
	submitTool     openaistool.SubmitMetadata[Response]
	genaiMetrics   *metrics.GenAI
	retryConfig    retry.RetryConfig
	resourceLabels map[string]string

	// resultValidators gate the terminal submit tool. When the model calls the
	// submit tool with a payload that parses, every validator runs concurrently
	// against the parsed response; any findings reject the submission back to
	// the model as the tool's result (the loop continues), and a validator
	// error aborts the run. Only when all validators accept does the response
	// commit and end the run. The chain always begins with the base
	// schema-conformance validator (schema.ResultValidator), which holds the
	// response to the constraints its jsonschema struct tags declare; callers
	// append semantic validators via WithResultValidator (repeatable).
	resultValidators []callbacks.ResultValidator[Response]

	// toolCallConcurrency bounds how many of a single turn's tool calls run
	// concurrently when the model emits more than one (parallel tool calls).
	// Defaults to DefaultToolCallConcurrency. A value of 1 forces strictly
	// sequential dispatch. Concurrent dispatch is only safe when the registered
	// tool handlers are themselves safe for concurrent use (they share the
	// trace, which is safe). Set via WithToolCallConcurrency.
	toolCallConcurrency int
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
		client:              client,
		modelName:           "google/gemini-2.5-flash",
		prompt:              prompt,
		maxTokens:           8192,
		maxTurns:            DefaultMaxTurns,
		temperature:         0.1,
		genaiMetrics:        metrics.NewGenAI("chainguard.ai.agents"),
		retryConfig:         retry.DefaultRetryConfig(),
		toolCallConcurrency: DefaultToolCallConcurrency,

		// The base schema-conformance validator is always first: submissions
		// must honor the constraints declared in the Response type's
		// jsonschema tags before any caller-registered validator runs.
		resultValidators: []callbacks.ResultValidator[Response]{schema.ResultValidator[Response]()},
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

	// Append the static user prompt suffix, when configured. The
	// OpenAI-compatible API has no per-block prompt-cache semantics, so plain
	// concatenation preserves the prompt content without any block layout.
	prompt, err = appendUserPromptSuffix(prompt, e.userPromptSuffix)
	if err != nil {
		return response, err
	}

	trace, done := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() {
		done(response, err)
	}()

	clog.InfoContext(ctx, "Starting OpenAI-compatible agent execution",
		"model", e.modelName,
		"prompt_length", len(prompt),
	)

	// submitToolName is the configured terminal tool the model calls to
	// return its result. Empty when no submit tool is registered.
	submitToolName := e.submitToolName()

	// Build tool definitions.
	toolDefs := make([]openai.ChatCompletionToolParam, 0, len(tools)+1)
	for _, meta := range tools {
		toolDefs = append(toolDefs, meta.Definition)
	}
	// Advertise the terminal submit tool alongside the regular tools. It lives
	// outside the tools map — dispatch routes it through evaluateSubmission —
	// but the model discovers it the same way. A caller-registered tool with
	// the same name takes precedence, matching dispatch.
	if submitToolName != "" {
		if _, exists := tools[submitToolName]; !exists {
			toolDefs = append(toolDefs, e.submitTool.Definition)
		}
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

	// executeToolCall runs a single tool call and returns its serialized result.
	// The handler writes any terminal result into resultPtr; each tool call in a
	// turn gets its own slot so concurrent handlers never race on the same
	// pointer. The committed return reports that the terminal submit tool
	// accepted the call and the registered result validators passed, so
	// resultPtr holds the run's final result — even when that result is the
	// zero value.
	executeToolCall := func(tc openai.ChatCompletionMessageToolCall, resultPtr *Response) (string, bool, error) {
		kvs := []any{"tool", tc.Function.Name, "id", tc.ID}
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
			for k, v := range args {
				kvs = append(kvs, "args."+k, v)
			}
		}
		clog.InfoContext(ctx, "Executing tool call", kvs...)

		var res map[string]any
		committed := false
		switch meta, ok := tools[tc.Function.Name]; {
		case ok:
			e.recordToolCall(ctx, tc.Function.Name)
			res = meta.Handler(ctx, tc, trace, resultPtr)
			// Preserve the model's universal `reasoning` argument on the
			// recorded call (handlers record curated param maps that drop it).
			if r, ok := args["reasoning"].(string); ok {
				trace.AttachToolCallReasoning(tc.ID, r)
			}
		case tc.Function.Name == submitToolName && e.submitTool.Handler != nil:
			// Terminal submit tool: parse the call, gate the parsed response
			// on the registered result validators, and only then commit it as
			// the run's final result. A rejected submission returns the
			// validators' findings as the tool result so the model can address
			// them and submit again — the loop continues.
			e.recordToolCall(ctx, tc.Function.Name)
			var err error
			res, committed, err = e.evaluateSubmission(ctx, tc, args, trace, resultPtr)
			if err != nil {
				return "", false, err
			}
		default:
			clog.ErrorContext(ctx, "Unknown tool requested", "tool", tc.Function.Name)
			trace.BadToolCall(tc.ID, tc.Function.Name,
				map[string]any{"arguments": tc.Function.Arguments},
				fmt.Errorf("unknown tool: %q", tc.Function.Name))
			res = map[string]any{"error": fmt.Sprintf("unknown tool: %q", tc.Function.Name)}
		}

		resBytes, err := json.Marshal(res)
		if err != nil {
			return "", false, fmt.Errorf("failed to marshal tool result: %w", err)
		}
		return string(resBytes), committed, nil
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
				// Gated on the WithPayloadsEnabled opt-in inside AppendReasoning:
				// raw thinking is confidential completion content, not structural
				// metadata, so it is only captured when payloads are enabled.
				trace.AppendReasoning(agenttrace.ReasoningContent{
					Thinking: thinking,
				})
			}
		}

		// Handle tool calls.
		if len(choice.Message.ToolCalls) > 0 {
			// Add assistant message with tool calls to conversation.
			reqParams.Messages = append(reqParams.Messages, choice.Message.ToParam())

			// Dispatch the turn's tool calls under a bounded pool, collecting all
			// results before checking for a final result so the conversation
			// history stays consistent (every tool result message is appended).
			// The model may emit several independent tool calls in one turn
			// (parallel tool calls); a concurrency of 1 runs them strictly in
			// order, higher values run them concurrently. Each handler writes into
			// its own result slot so the shared finalResultPtr is never raced, and
			// the tool messages are appended in the model's original order. Tool
			// handlers must be safe for concurrent use when concurrency exceeds 1.
			toolCalls := choice.Message.ToolCalls
			type toolOutcome struct {
				msg       openai.ChatCompletionMessageParamUnion
				committed bool
				err       error
			}
			outcomes := make([]toolOutcome, len(toolCalls))
			perCallResults := make([]Response, len(toolCalls))

			g := new(errgroup.Group)
			g.SetLimit(max(1, e.toolCallConcurrency))
			for i, tc := range toolCalls {
				g.Go(func() error {
					resJSON, committed, cerr := executeToolCall(tc, &perCallResults[i])
					if cerr != nil {
						outcomes[i] = toolOutcome{err: cerr}
						return nil
					}
					outcomes[i] = toolOutcome{msg: openai.ToolMessage(resJSON, tc.ID), committed: committed}
					return nil
				})
			}
			_ = g.Wait()

			for i := range toolCalls {
				if outcomes[i].err != nil {
					return response, true, outcomes[i].err
				}
				reqParams.Messages = append(reqParams.Messages, outcomes[i].msg)
			}
			for i := range toolCalls {
				// The committed flag is the submit tool's explicit terminal
				// signal — it fires even for a zero-value result, so the model
				// is never told "submitted successfully" on a run that keeps
				// going. The zero-value check preserves the legacy contract
				// for regular tools that write a non-zero result through
				// their result pointer.
				if outcomes[i].committed || !reflect.ValueOf(perCallResults[i]).IsZero() {
					clog.InfoContext(ctx, "Tool set final result, exiting conversation loop", "turns_completed", turn+1)
					e.recordTurns(ctx, turn+1, false)
					return perCallResults[i], true, nil
				}
			}
			return response, false, nil
		}

		textContent := choice.Message.Content

		// When submit_result is configured, redirect text responses back to the tool.
		if e.submitTool.Handler != nil && textContent != "" {
			clog.WarnContext(ctx, "Model responded with text instead of calling submit_result, redirecting")
			e.recordToolCall(ctx, "submit_result_redirect")

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

// submitToolName returns the configured terminal submit tool's name, or ""
// when no submit tool is registered. The default name "submit_result" is used
// when a submit tool is registered without an explicit name.
func (e *executor[Request, Response]) submitToolName() string {
	if e.submitTool.Handler == nil {
		return ""
	}
	if e.submitTool.Definition.Function.Name != "" {
		return e.submitTool.Definition.Function.Name
	}
	return "submit_result"
}

// evaluateSubmission runs the terminal submit tool handler for a single call
// and gates its accepted response on the registered result validators. It
// returns the tool result to send back to the model and whether the response
// committed as the run's final result (written through resultPtr). A rejected
// submission returns the validators' findings so the model can address them
// and submit again; a validator error aborts the run.
func (e *executor[Request, Response]) evaluateSubmission(
	ctx context.Context,
	tc openai.ChatCompletionMessageToolCall,
	args map[string]any,
	trace *agenttrace.Trace[Response],
	resultPtr *Response,
) (map[string]any, bool, error) {
	outcome := e.submitTool.Handler(ctx, tc, trace)
	if !outcome.Accepted {
		// The handler recorded the failed call on the trace; its ToolResult
		// carries the parameter/parse error back to the model.
		return outcome.ToolResult, false, nil
	}

	// The handler leaves accepted calls unrecorded so this trace call's
	// completion reflects the validation verdict.
	toolCall := trace.StartToolCall(tc.ID, tc.Function.Name, args)

	findings, err := callbacks.ValidateResult(ctx, e.resultValidators, outcome.Response, outcome.Reasoning)
	if err != nil {
		err = fmt.Errorf("result validation: %w", err)
		toolCall.Complete(nil, err)
		return nil, false, err
	}
	if len(findings) > 0 {
		clog.InfoContext(ctx, "Submission rejected by result validators", "findings", len(findings))
		e.recordToolCall(ctx, "submit_result_rejected")
		toolCall.Complete(nil, fmt.Errorf("result rejected: validation raised %d finding(s)", len(findings)))
		return callbacks.RejectionResult(e.submitToolName(), findings), false, nil
	}

	*resultPtr = outcome.Response
	toolCall.Complete(outcome.ToolResult, nil)
	return outcome.ToolResult, true, nil
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

// appendUserPromptSuffix appends the built suffix to the prompt with a
// blank-line separator. A nil suffix returns the prompt unchanged. The suffix
// must be fully bound; a Build failure (for example an unbound placeholder)
// is returned wrapped.
func appendUserPromptSuffix(prompt string, suffix *promptbuilder.Prompt) (string, error) {
	if suffix == nil {
		return prompt, nil
	}
	built, err := suffix.Build()
	if err != nil {
		return "", fmt.Errorf("failed to build user prompt suffix: %w", err)
	}
	return prompt + "\n\n" + built, nil
}
