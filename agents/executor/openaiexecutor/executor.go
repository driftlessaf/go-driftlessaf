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
	"slices"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
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
	"github.com/openai/openai-go/shared"
)

// Interface is the public interface for OpenAI-compatible agent execution.
type Interface[Request promptbuilder.Bindable, Response any] interface {
	// Execute runs the agent conversation with the given request and tools.
	Execute(ctx context.Context, request Request, tools map[string]openaistool.Metadata[Response]) (Response, error)
}

// DefaultMaxTurns is the default maximum number of conversation turns before aborting.
const DefaultMaxTurns = 200

// maxInvalidResponseRetries bounds how many consecutive unusable responses the
// executor will nudge the model through before failing. An unusable response is
// either a tool-call turn whose arguments are not JSON objects or a normal-stop
// turn carrying neither text nor tool calls.
const maxInvalidResponseRetries = 3

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

	maxTokens       int64
	maxTurns        int
	temperature     float64
	omitTemperature bool
	reasoningEffort shared.ReasoningEffort // "" = unset; omitted from requests
	provider        Provider
	tokenLimitParam TokenLimitParameter
	submitTool      openaistool.SubmitMetadata[Response]
	telemetry       *telemetry.Recorder
	retryConfig     retry.RetryConfig
	resourceLabels  map[string]string
	attribution     agenttrace.Attribution

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
		client:      client,
		modelName:   "google/gemini-2.5-flash",
		prompt:      prompt,
		maxTokens:   8192,
		maxTurns:    DefaultMaxTurns,
		temperature: 0.1,
		provider:    ProviderOpenAICompatible,
		attribution: agenttrace.Attribution{
			ProviderName: ProviderOpenAICompatible.metricName(),
			System:       ProviderOpenAICompatible.traceSystem(),
		},
		tokenLimitParam:     TokenLimitMaxCompletionTokens,
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

	// The recorder is built after options so it captures the final model and
	// resource labels. codeFromError is nil because this executor does not
	// record genai.api.requests: it has no error→code mapping yet, and wiring
	// one up is a behavior change tracked separately.
	e.telemetry = telemetry.NewRecorder(metrics.NewGenAI("chainguard.ai.agents"), e.modelName, e.attribution.ProviderName, e.resourceLabels, nil)

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
	prompt, err = execshared.AppendUserPromptSuffix(prompt, e.userPromptSuffix)
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

	reqParams := e.requestParams(messages, toolDefs)

	// isSubmit reports whether a call routes to the terminal submit tool. It
	// is the routing predicate consulted by executeToolCall's dispatch switch.
	isSubmit := execshared.SubmitPredicate(tools, submitToolName, e.submitTool.Handler != nil)

	// heldOutTools is the set of tool names dispatched sequentially after the
	// concurrent tool pool drains, rather than within it (see
	// execshared.DispatchToolCalls). A held-out call's work runs only once
	// every sibling handler has finished and its real tool_result is in the
	// transcript. Today the only member is the terminal submit tool — its
	// result validators may read state the sibling handlers produce
	// (worktrees, files), so they must observe the finished state rather than
	// race the handlers still producing it — and only when the call actually
	// routes to submit (a caller tool of the same name shadows it and is
	// dispatched in the pool, so it is not held out). Membership is derived
	// from isSubmit so the two predicates cannot drift. Building the partition
	// as a set is the seam DEV-2247 widens: the suspend tool joins this set so
	// it too quiesces behind its siblings' real tool_results.
	heldOutTools := make(map[string]struct{}, 1)
	if isSubmit(submitToolName) {
		heldOutTools[submitToolName] = struct{}{}
	}
	heldOut := func(name string) bool {
		_, ok := heldOutTools[name]
		return ok
	}

	// availableTools names every tool the model can call, for the unknown-tool
	// result. The tool set is fixed for the run, so it is built once here
	// rather than per bad call. isSubmit gates the held-out submit name, so a
	// name that no option configured, or that a caller-registered tool
	// shadows, is not listed as its own entry.
	heldOutNames := make([]string, 0, 1)
	if isSubmit(submitToolName) {
		heldOutNames = append(heldOutNames, submitToolName)
	}
	availableTools := availableToolNames(tools, heldOutNames...)
	invalidResponseRetries := 0

	// retryInvalidResponse appends a provider-neutral correction without
	// replaying the unusable assistant response. Replaying malformed tool-call
	// arguments is not safe: strict OpenAI-compatible gateways reject the
	// conversation history before the model can see a tool-result correction.
	retryInvalidResponse := func(turn int, reason, instruction string) error {
		invalidResponseRetries++
		if invalidResponseRetries > maxInvalidResponseRetries {
			return fmt.Errorf("model returned an unusable response repeatedly (%s) after %d attempts", reason, invalidResponseRetries)
		}
		clog.WarnContext(ctx, "Model returned an unusable response, asking it to retry",
			"reason", reason,
			"attempt", invalidResponseRetries,
			"turn", turn)
		reqParams.Messages = append(reqParams.Messages, openai.UserMessage(instruction))
		return nil
	}

	// executeToolCall runs a single tool call and returns its serialized result.
	// The handler writes any terminal result into resultPtr; each tool call in a
	// turn gets its own slot so concurrent handlers never race on the same
	// pointer. The committed return reports that the terminal submit tool
	// accepted the call and the registered result validators passed, so
	// resultPtr holds the run's final result — even when that result is the
	// zero value.
	executeToolCall := func(tc openai.ChatCompletionMessageToolCall, args map[string]any, resultPtr *Response) (string, bool, error) {
		kvs := []any{"tool", tc.Function.Name, "id", tc.ID}
		for k, v := range args {
			kvs = append(kvs, "args."+k, v)
		}
		clog.InfoContext(ctx, "Executing tool call", kvs...)

		var res map[string]any
		committed := false
		switch meta, ok := tools[tc.Function.Name]; {
		case ok:
			e.telemetry.RecordToolCall(ctx, tc.Function.Name)
			res = meta.Handler(ctx, tc, trace, resultPtr)
			// Preserve the model's universal `reasoning` argument on the
			// recorded call (handlers record curated param maps that drop it).
			if r, ok := args["reasoning"].(string); ok {
				trace.AttachToolCallReasoning(tc.ID, r)
			}
		case isSubmit(tc.Function.Name):
			// Terminal submit tool: parse the call, gate the parsed response
			// on the registered result validators, and only then commit it as
			// the run's final result. A rejected submission returns the
			// validators' findings as the tool result so the model can address
			// them and submit again — the loop continues.
			e.telemetry.RecordToolCall(ctx, tc.Function.Name)
			var err error
			res, committed, err = e.evaluateSubmission(ctx, tc, args, trace, resultPtr)
			if err != nil {
				return "", false, err
			}
		default:
			clog.ErrorContext(ctx, "Unknown tool requested", "tool", tc.Function.Name)
			trace.BadToolCall(tc.ID, tc.Function.Name,
				map[string]any{"arguments": tc.Function.Arguments},
				fmt.Errorf("%w: %q", agenttrace.ErrUnknownTool, tc.Function.Name))
			res = map[string]any{
				"error":           fmt.Sprintf("unknown tool: %q", tc.Function.Name),
				"available_tools": availableTools,
			}
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
		llmTurn := trace.BeginTurnWithAttribution(turn, e.modelName, e.attribution)
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
			e.telemetry.RecordTokens(ctx, completion.Usage.PromptTokens, completion.Usage.CompletionTokens)
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
			toolCalls := choice.Message.ToolCalls
			parsedArgs := make([]map[string]any, len(toolCalls))

			// Validate the complete batch before appending it to history or running
			// any handler. Some compatible APIs accept malformed arguments in a
			// completion but reject those same arguments when the assistant message
			// is replayed on the next request. Rejecting the batch atomically also
			// prevents valid siblings from producing side effects when another call
			// cannot be represented in a valid transcript.
			var malformed []string
			for i, tc := range toolCalls {
				args, err := decodeToolArguments(tc.Function.Arguments)
				if err != nil {
					malformed = append(malformed, tc.Function.Name)
					trace.RejectedToolCall(tc.ID, tc.Function.Name,
						map[string]any{"arguments_bytes": len(tc.Function.Arguments)},
						fmt.Errorf("invalid tool arguments: %w", err))
					continue
				}
				parsedArgs[i] = args
			}
			if len(malformed) > 0 {
				if err := retryInvalidResponse(turn,
					fmt.Sprintf("invalid JSON arguments for tools %v", malformed),
					fmt.Sprintf("One or more tool calls contained invalid JSON arguments, so no tools were executed. Try the complete tool-call turn again using JSON objects for the available tools: %v.", availableTools)); err != nil {
					return response, true, err
				}
				return response, false, nil
			}
			invalidResponseRetries = 0

			// Add assistant message with tool calls to conversation.
			reqParams.Messages = append(reqParams.Messages, choice.Message.ToParam())

			// Dispatch the turn's tool calls under a bounded pool, collecting all
			// results before checking for a final result so the conversation
			// history stays consistent (every tool result message is appended).
			// Submit calls are held out until the pool drains — see
			// execshared.DispatchToolCalls for the concurrency and
			// submit-quiesce semantics. Each handler writes into its own result
			// slot so the shared finalResultPtr is never raced, and the tool
			// messages are appended in the model's original order. Tool handlers
			// must be safe for concurrent use when concurrency exceeds 1.
			type toolOutcome struct {
				msg       openai.ChatCompletionMessageParamUnion
				committed bool
				err       error
			}
			outcomes := make([]toolOutcome, len(toolCalls))
			perCallResults := make([]Response, len(toolCalls))

			execshared.DispatchToolCalls(toolCalls, e.toolCallConcurrency,
				func(tc openai.ChatCompletionMessageToolCall) bool { return heldOut(tc.Function.Name) },
				func(i int, tc openai.ChatCompletionMessageToolCall) {
					resJSON, committed, cerr := executeToolCall(tc, parsedArgs[i], &perCallResults[i])
					if cerr != nil {
						outcomes[i] = toolOutcome{err: cerr}
						return
					}
					outcomes[i] = toolOutcome{msg: openai.ToolMessage(resJSON, tc.ID), committed: committed}
				})

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
					e.telemetry.RecordTurns(ctx, turn+1, false)
					return perCallResults[i], true, nil
				}
			}
			return response, false, nil
		}

		textContent := choice.Message.Content
		if textContent != "" {
			invalidResponseRetries = 0
		}

		// When submit_result is configured, redirect text responses back to the tool.
		if e.submitTool.Handler != nil && textContent != "" {
			clog.WarnContext(ctx, "Model responded with text instead of calling submit_result, redirecting")
			e.telemetry.RecordToolCall(ctx, "submit_result_redirect")

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
			e.telemetry.RecordTurns(ctx, turn+1, false)
			return resp, true, nil
		}

		// A natural-stop response with neither text nor tool calls is transient
		// for several OpenAI-compatible reasoning models. Preserve the valid
		// conversation built so far and nudge the model, but never retry explicit
		// terminal reasons such as length or content_filter.
		switch choice.FinishReason {
		case "", "stop":
			if err := retryInvalidResponse(turn,
				"no content and no tool calls",
				"Your last response was empty. Please continue: call one of the available tools or provide your answer as text."); err != nil {
				return response, true, err
			}
			return response, false, nil
		default:
			return response, true, fmt.Errorf("no content in completion response (finish_reason=%q)", choice.FinishReason)
		}
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
	e.telemetry.RecordTurns(ctx, e.maxTurns, true)
	return response, fmt.Errorf("agent exceeded maximum conversation turns (%d)", e.maxTurns)
}

// decodeToolArguments validates the wire-level function arguments before a
// tool-call turn enters conversation history. The OpenAI-compatible contract
// requires an object, not merely any syntactically valid JSON value.
func decodeToolArguments(arguments string) (map[string]any, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	return args, nil
}

func (e *executor[Request, Response]) requestParams(
	messages []openai.ChatCompletionMessageParamUnion,
	tools []openai.ChatCompletionToolParam,
) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model:    e.modelName,
		Messages: messages,
		Tools:    tools,
		// The zero value is omitted from the request (omitzero), so this only
		// takes effect when WithEffort configured it.
		ReasoningEffort: e.reasoningEffort,
	}
	if !e.omitTemperature {
		params.Temperature = param.NewOpt(e.temperature)
	}
	if e.tokenLimitParam == TokenLimitMaxTokens {
		params.MaxTokens = param.NewOpt(e.maxTokens)
	} else {
		params.MaxCompletionTokens = param.NewOpt(e.maxTokens)
	}
	return params
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
// and gates its accepted response on the registered result validators via
// execshared.GateSubmission (see there for the gate semantics). It returns
// the tool result to send back to the model and whether the response
// committed as the run's final result (written through resultPtr).
func (e *executor[Request, Response]) evaluateSubmission(
	ctx context.Context,
	tc openai.ChatCompletionMessageToolCall,
	args map[string]any,
	trace *agenttrace.Trace[Response],
	resultPtr *Response,
) (map[string]any, bool, error) {
	return execshared.GateSubmission(ctx, e.submitTool.Handler(ctx, tc, trace),
		trace, tc.ID, tc.Function.Name, args,
		e.resultValidators, e.telemetry, e.submitToolName(), resultPtr)
}

// availableToolNames returns the sorted names of every tool the model can
// call: the registered tools, plus the heldOut names that dispatch outside
// the tool map (the terminal submit tool). Callers pass a held-out name only
// while it routes, so a shadowed or unconfigured name never reaches the list.
// The unknown-tool result carries the list, so a model that misspells or
// invents a tool name can correct the call.
func availableToolNames[Meta any](tools map[string]Meta, heldOut ...string) []string {
	names := make([]string, 0, len(tools)+len(heldOut))
	for name := range tools {
		names = append(names, name)
	}
	names = append(names, heldOut...)
	slices.Sort(names)
	return slices.Compact(names)
}
