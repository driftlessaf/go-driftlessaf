/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/result"
	"chainguard.dev/driftlessaf/agents/schema"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
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

// DefaultMaxTurns is the default maximum number of conversation turns (LLM
// round-trips) before the executor aborts. Each turn corresponds to one
// Gemini API call. This prevents runaway loops when the model keeps calling
// tools without converging on a result.
const DefaultMaxTurns = 200

// DefaultToolCallConcurrency is the default bound on how many of a single
// turn's tool calls run concurrently. Models routinely emit several
// independent function calls in one turn (parallel function calling);
// dispatching their handlers concurrently cuts wall-clock latency. Override
// with WithToolCallConcurrency — a value of 1 restores strictly sequential
// dispatch.
const DefaultToolCallConcurrency = 10

// executor is the private implementation of Interface
type executor[Request promptbuilder.Bindable, Response any] struct {
	client             *genai.Client
	prompt             *promptbuilder.Prompt
	model              string
	temperature        float32
	maxOutputTokens    int32
	maxTurns           int // maximum conversation turns before aborting
	systemInstructions *promptbuilder.Prompt

	// userPromptSuffix, when non-nil, is a static operator-authored prompt
	// appended to the built user prompt with a blank-line separator. See
	// WithUserPromptSuffix; the request is never bound into it.
	userPromptSuffix *promptbuilder.Prompt

	responseMIMEType string
	responseSchema   *genai.Schema
	thinkingBudget   *int32                              // nil = disabled, non-nil = enabled with budget
	submitTool       googletool.SubmitMetadata[Response] // opt-in: set via WithSubmitResultProvider
	telemetry        *telemetry.Recorder                 // shared GenAI metrics recorder, built after options in New
	retryConfig      retry.RetryConfig                   // retry configuration for transient Vertex AI errors
	resourceLabels   map[string]string                   // resource labels for GCP billing attribution

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

	// cacheControl enables Vertex AI context caching. When true, the executor
	// creates a CachedContent resource containing system instructions and tool
	// definitions, then references it in GenerateContentConfig instead of setting
	// SystemInstruction and Tools directly. Cached tokens are served at reduced
	// cost with a configurable TTL.
	// Enabled by default — disable with WithoutCacheControl() if needed.
	// See: https://cloud.google.com/vertex-ai/generative-ai/docs/context-cache/context-cache-overview
	cacheControl bool

	// cacheTTL is the time-to-live for cached content resources.
	// Default: 30 minutes. Can be overridden via WithCacheTTL().
	cacheTTL time.Duration

	// cacheMu protects all cache-related mutable state below.
	cacheMu             sync.Mutex
	cachedContentName   string    // resource name of active CachedContent ("" = none)
	cachedContentExpiry time.Time // when the current cache expires

	// toolCallConcurrency bounds how many of a single turn's function calls run
	// concurrently when the model emits more than one (parallel function
	// calling). Defaults to DefaultToolCallConcurrency. A value of 1 runs the
	// turn's function calls strictly in order, one at a time. Concurrent
	// dispatch is only safe when the registered tool handlers are themselves
	// safe for concurrent use (they share the trace, which is safe). Set via
	// WithToolCallConcurrency.
	toolCallConcurrency int
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
		client:              client,
		prompt:              prompt,
		model:               "gemini-2.5-flash",         // Default to Gemini 2.5 Flash
		temperature:         0.1,                        // Default temperature for consistency
		maxOutputTokens:     8192,                       // Default max tokens
		maxTurns:            DefaultMaxTurns,            // Default max conversation turns
		retryConfig:         retry.DefaultRetryConfig(), // Default retry config for rate limit handling
		cacheControl:        true,                       // Context caching on by default — see cacheControl field comment
		cacheTTL:            30 * time.Minute,           // Default cache TTL
		toolCallConcurrency: DefaultToolCallConcurrency, // Concurrent tool dispatch by default — see WithToolCallConcurrency

		// The base schema-conformance validator is always first: submissions
		// must honor the constraints declared in the Response type's
		// jsonschema tags before any caller-registered validator runs.
		resultValidators: []callbacks.ResultValidator[Response]{schema.ResultValidator[Response]()},
	}

	// Apply options
	for _, opt := range options {
		if err := opt(exec); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	// The recorder is built after options so it captures the final model and
	// resource labels.
	exec.telemetry = telemetry.NewRecorder(genaiMetrics, exec.model, "gcp.vertex_ai", exec.resourceLabels, responseCodeFromError)

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
	// Guard against incompatible combination: thinking mode + seed tool calls.
	// When ThinkingConfig is set, Gemini requires that model turns with FunctionCall
	// parts also include Thought parts. Synthetic seed turns have no Thought parts,
	// causing a Vertex AI API validation error.
	if e.thinkingBudget != nil && len(seedToolCalls) > 0 {
		return resp, errors.New("seed tool calls cannot be used with thinking mode: " +
			"synthetic function call history is missing required thought blocks")
	}

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

	// Append the static user prompt suffix, when configured. Gemini has no
	// per-block prompt-cache semantics (context caching covers system
	// instructions and tools via CachedContent), so plain concatenation
	// preserves the prompt content without any block layout.
	prompt, err = execshared.AppendUserPromptSuffix(prompt, e.userPromptSuffix)
	if err != nil {
		return resp, err
	}

	// Start a trace for this execution — done completes and records
	trace, done := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() {
		done(resp, err)
	}()

	// submitToolName is the configured terminal tool the model calls to
	// return its result. Empty when no submit tool is registered.
	submitToolName := e.submitToolName()

	// Build tool definitions, sorted by name for deterministic ordering.
	//
	// Why sort? Vertex AI context caching hashes the cached content (system
	// instructions + tools). Go maps iterate in non-deterministic order, so
	// without sorting, the tool definitions could serialize differently on each
	// call — producing different cache content even though the tools haven't
	// changed. Sorting by name ensures stable content across calls.
	toolDeclarations := make([]*genai.FunctionDeclaration, 0, len(tools)+1)
	for _, meta := range tools {
		toolDeclarations = append(toolDeclarations, meta.Definition)
	}
	// Advertise the terminal submit tool alongside the regular tools. It lives
	// outside the tools map — dispatch routes it through evaluateSubmission —
	// but the model discovers it the same way. A caller-registered tool with
	// the same name takes precedence, matching dispatch.
	if submitToolName != "" {
		if _, exists := tools[submitToolName]; !exists {
			toolDeclarations = append(toolDeclarations, e.submitTool.Definition)
		}
	}
	slices.SortFunc(toolDeclarations, func(a, b *genai.FunctionDeclaration) int {
		return cmp.Compare(a.Name, b.Name)
	})

	// Create generation config
	config := &genai.GenerateContentConfig{
		Temperature:     ptr(e.temperature),
		MaxOutputTokens: e.maxOutputTokens,
		Labels:          e.resourceLabels,
	}

	// Build system instruction content
	var systemInstruction *genai.Content
	if e.systemInstructions != nil {
		systemPrompt, err := e.systemInstructions.Build()
		if err != nil {
			return resp, fmt.Errorf("building system prompt: %w", err)
		}
		systemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: systemPrompt}},
		}
	}

	// Build tools
	var genaiTools []*genai.Tool
	if len(toolDeclarations) > 0 {
		genaiTools = []*genai.Tool{{FunctionDeclarations: toolDeclarations}}
	}

	// Attempt to use cached content for system instructions + tools.
	// When using CachedContent, SystemInstruction and Tools MUST NOT be set
	// on GenerateContentConfig — they are already in the cache.
	usedCache := false
	if e.cacheControl && (systemInstruction != nil || len(genaiTools) > 0) {
		cacheName, err := e.getOrCreateCache(ctx, systemInstruction, genaiTools)
		if err != nil {
			clog.WarnContext(ctx, "Failed to create cached content, falling back to non-cached mode", "error", err)
		} else {
			config.CachedContent = cacheName
			usedCache = true
		}
	}

	// Fall back to inline system instruction and tools if not using cache.
	if !usedCache {
		if systemInstruction != nil {
			config.SystemInstruction = systemInstruction
		}
		if len(genaiTools) > 0 {
			config.Tools = genaiTools
		}
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
	clog.InfoContext(ctx, "Creating Google AI chat session",
		"model", e.model,
	)

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

	// isSubmit reports whether a call routes to the terminal submit tool. It
	// is the single routing predicate: executeToolCall's dispatch switch and
	// the turn loop's held-out-of-pool partition both use it, so the two
	// sites cannot drift.
	isSubmit := execshared.SubmitPredicate(tools, submitToolName, e.submitTool.Handler != nil)

	// executeToolCall runs a single function call and returns its response
	// part. The handler writes any terminal result into resultPtr; each call
	// gets its own slot on the concurrent path so handlers never race on the
	// same pointer. The committed return reports that the terminal submit tool
	// accepted the call and the registered result validators passed, so
	// resultPtr holds the run's final result — even when that result is the
	// zero value.
	executeToolCall := func(call *genai.FunctionCall, resultPtr *Response) (*genai.Part, bool, error) {
		kvs := make([]any, 0, 4+2*len(call.Args))
		kvs = append(kvs, "tool", call.Name, "id", call.ID)
		for k, v := range call.Args {
			kvs = append(kvs, "args."+k, v)
		}
		clog.InfoContext(ctx, "Executing tool call", kvs...)

		// Record tool call metric
		e.telemetry.RecordToolCall(ctx, call.Name)

		// Find and execute the handler for this tool
		var toolResponse *genai.FunctionResponse
		committed := false
		switch toolMeta, found := tools[call.Name]; {
		case found:
			// Execute the tool handler
			toolResponse = toolMeta.Handler(ctx, call, trace, resultPtr)
			// Preserve the model's universal `reasoning` argument on
			// the recorded call (handlers record curated param maps
			// that drop it).
			if r, ok := call.Args["reasoning"].(string); ok {
				trace.AttachToolCallReasoning(call.ID, r)
			}
		case isSubmit(call.Name):
			// Terminal submit tool: parse the call, gate the parsed response
			// on the registered result validators, and only then commit it as
			// the run's final result. A rejected submission returns the
			// validators' findings as the tool result so the model can address
			// them and submit again — the loop continues.
			result, com, err := e.evaluateSubmission(ctx, call, trace, resultPtr)
			if err != nil {
				return nil, false, err
			}
			committed = com
			toolResponse = &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result}
		default:
			clog.ErrorContext(ctx, "Unknown function call requested by model", "function", call.Name)
			toolResponse = googletool.Error(call, "Unknown function: %s", call.Name)

			// Record bad tool call for unknown function
			trace.BadToolCall(call.ID, call.Name, call.Args, fmt.Errorf("unknown function: %q", call.Name))
		}

		return &genai.Part{FunctionResponse: toolResponse}, committed, nil
	}

	// Execute seed tool calls and build complete history
	for _, call := range seedToolCalls {
		clog.InfoContext(ctx, "Pre-executing seed tool call", "tool", call.Name, "id", call.ID)

		part, committed, err := executeToolCall(call, finalResultPtr)
		if err != nil {
			return resp, err
		}

		// Check if a tool set the final result during seed execution. The
		// committed flag is the submit tool's explicit signal; the zero-value
		// check preserves the legacy contract for regular tools that write a
		// non-zero result through their result pointer.
		if committed || !reflect.ValueOf(finalResult).IsZero() {
			clog.InfoContext(ctx, "Seed tool set final result, exiting immediately")
			return finalResult, nil
		}

		// Add model response with function call and function response
		history = append(history, &genai.Content{
			Role: "model",
			Parts: []*genai.Part{{
				FunctionCall: call,
			}},
		}, &genai.Content{
			Role:  "user",
			Parts: []*genai.Part{part},
		})
	}

	// Create chat with first n-1 messages, send last message separately
	chat, err := e.client.Chats.Create(ctx, e.model, config, history[:len(history)-1])
	if err != nil {
		return resp, fmt.Errorf("failed to create chat with model %q: %w", e.model, err)
	}

	// Send final message to get response with retry for transient errors
	clog.InfoContext(ctx, "Sending final message")
	response, err := e.sendWithRetry(ctx, e.telemetry.WithAPIRequestCounter(ctx, e.retryConfig), "send_initial_message", "failed to send final message", func() (*genai.GenerateContentResponse, error) {
		return chat.Send(ctx, history[len(history)-1].Parts...)
	})
	if err != nil {
		return resp, err
	}

	// Handle the conversation loop with bounded turns to prevent runaway executions.
	var responseText string
	// The named err return is load-bearing: the deferred Fail call below reads
	// it at function exit. Every error path must use `return ..., err` (or set
	// the named err before bare-returning) — a bare return inside a nested
	// block where err is shadowed via `:=` would silently bypass Fail.
	executeTurn := func(turn int, response *genai.GenerateContentResponse) (_ Response, _ *genai.GenerateContentResponse, _ bool, err error) {
		var zero Response
		llmTurn := trace.BeginTurn(turn, agenttrace.SystemGoogleVertex, e.model)
		defer func() {
			if err != nil {
				llmTurn.Fail(err)
			}
			llmTurn.End()
		}()

		// Per-turn retry config wires transient API errors that the retry
		// recovers from into the turn's Errors list. Without this, retries
		// that eventually succeed leave no trace of the transients in BQ.
		// Used by all in-turn sendWithRetry call sites below.
		// Also count every retried attempt in genai.api.requests; the final
		// attempt is counted by sendWithRetry after RetryWithBackoff.
		turnCfg := e.retryConfig
		turnCfg.OnAttemptError = llmTurn.RecordError
		turnCfg = e.telemetry.WithAPIRequestCounter(ctx, turnCfg)

		// Record tokens for the response being processed in this turn.
		// Unlike claude/openai executors (which record tokens right after
		// each API call), the Google SDK loop processes the *previous*
		// iteration's response at the top of the next turn — so for turn 0
		// this captures the initial API call, and for later turns it captures
		// the response from the preceding tool/redirect call.
		if response != nil && response.UsageMetadata != nil {
			llmTurn.RecordTokens(
				int64(response.UsageMetadata.PromptTokenCount),
				int64(response.UsageMetadata.CandidatesTokenCount),
			)
			// Vertex reports cache hits via CachedContentTokenCount; cache
			// writes are not separately billed for Gemini (see cost view),
			// so CacheCreationTokens stays 0 here. A future "cache write
			// events" metric would have to source from getOrCreateCache
			// (the only place a Gemini cache create actually happens) —
			// turns[] sees only the read-side hits.
			if e.cacheControl && response.UsageMetadata.CachedContentTokenCount > 0 {
				llmTurn.RecordCacheTokens(int64(response.UsageMetadata.CachedContentTokenCount), 0)
			}
		}

		clog.InfoContext(ctx, "Received response from model", "candidates_count", len(response.Candidates))

		if len(response.Candidates) == 0 {
			return zero, nil, true, errors.New("no content generated - no candidates")
		}

		candidate := response.Candidates[0]

		// Capture per-turn LLM payload alongside RecordTokens. Unlike the
		// claude/openai executors (which capture params.Messages before the
		// SDK call), the genai chat handle owns conversation state — so we
		// read it from chat.History(false), which at this point includes the
		// just-returned response. PromptMessages therefore carries the full
		// context the model saw *plus* its own response; Completion holds
		// just the response candidate. The redundancy is benign — SQL queries
		// pick the column matching the question, and prompt_hash stays unique
		// per (context, response) pair. History(false) is uncurated so
		// malformed/filtered turns persist verbatim, matching the security
		// posture documented on agenttrace.SpanEventType. Gated inside on
		// agenttrace.WithPayloadsEnabled.
		if err := llmTurn.RecordRequest(chat.History(false)); err != nil {
			clog.WarnContext(ctx, "failed to record llm prompt payload", "error", err)
		}
		if err := llmTurn.RecordResponse(candidate.Content); err != nil {
			clog.WarnContext(ctx, "failed to record llm response payload", "error", err)
		}

		// Check for safety/blocking reasons before processing content
		if candidate.FinishReason == genai.FinishReasonSafety {
			clog.WarnContext(ctx, "Gemini blocked response due to safety filters",
				"finish_message", candidate.FinishMessage,
				"safety_ratings", candidate.SafetyRatings)
			return zero, nil, true, fmt.Errorf("response blocked by safety filters: %s", candidate.FinishMessage)
		}

		if candidate.FinishReason == genai.FinishReasonRecitation {
			clog.WarnContext(ctx, "Gemini blocked response due to recitation concerns",
				"finish_message", candidate.FinishMessage)
			return zero, nil, true, fmt.Errorf("response blocked due to recitation: %s", candidate.FinishMessage)
		}

		// Check for MAX_TOKENS - response was truncated due to output token limit
		if candidate.FinishReason == genai.FinishReasonMaxTokens {
			clog.WarnContext(ctx, "Gemini hit max output tokens limit, asking for more concise response",
				"finish_message", candidate.FinishMessage,
				"turn", turn)

			// Ask the model to provide a more concise version
			retryMsg := genai.Part{Text: "Your response exceeded the maximum output token limit. Please provide a more concise response that focuses on the most critical information while still completing the task."}
			retryResp, err := e.sendWithRetry(ctx, turnCfg, "send_max_tokens_retry", "failed to send retry message after hitting max tokens", func() (*genai.GenerateContentResponse, error) {
				return chat.SendMessage(ctx, retryMsg)
			})
			if err != nil {
				return zero, nil, true, err
			}

			// Continue with the new response
			return zero, retryResp, false, nil
		}

		// Check for malformed function call
		if candidate.FinishReason == genai.FinishReasonMalformedFunctionCall {
			clog.WarnContext(ctx, "Model attempted a malformed function call, asking it to retry",
				"finish_message", candidate.FinishMessage)

			// Build available function names for retry message
			var funcNames []string
			for _, decl := range toolDeclarations {
				funcNames = append(funcNames, decl.Name)
			}

			// Send a message asking the model to try again with retry for transient errors
			retryMsg := genai.Part{Text: fmt.Sprintf("The function call was malformed. Please try again using the available functions: %v", funcNames)}
			retryResp, err := e.sendWithRetry(ctx, turnCfg, "send_malformed_retry", "failed to send retry message after malformed function call", func() (*genai.GenerateContentResponse, error) {
				return chat.SendMessage(ctx, retryMsg)
			})
			if err != nil {
				return zero, nil, true, err
			}

			// Continue with the new response
			return zero, retryResp, false, nil
		}

		if candidate.Content == nil {
			clog.ErrorContext(ctx, "Gemini returned nil content",
				"finish_reason", candidate.FinishReason,
				"finish_message", candidate.FinishMessage)
			return zero, nil, true, fmt.Errorf("no content generated - candidate content is nil (finish_reason=%v, finish_message=%q)",
				candidate.FinishReason, candidate.FinishMessage)
		}

		if len(candidate.Content.Parts) == 0 {
			clog.ErrorContext(ctx, "Gemini returned empty parts",
				"finish_reason", candidate.FinishReason,
				"finish_message", candidate.FinishMessage)
			return zero, nil, true, fmt.Errorf("no content generated - no parts in candidate (finish_reason=%v, finish_message=%q)",
				candidate.FinishReason, candidate.FinishMessage)
		}

		// Check for function calls or text
		var toolCalls []*genai.FunctionCall
		var hasText bool

		for i, part := range candidate.Content.Parts {
			switch {
			case part.Thought:
				// Gated on the WithPayloadsEnabled opt-in inside AppendReasoning:
				// raw thinking is confidential completion content, not structural
				// metadata, so it is only captured when payloads are enabled.
				trace.AppendReasoning(agenttrace.ReasoningContent{
					Thinking: part.Text,
				})
				clog.InfoContext(ctx, "Found thought part",
					"part_index", i,
					"thinking_length", len(part.Text))
			case part.Text != "":
				responseText = part.Text
				hasText = true
				clog.InfoContext(ctx, "Found text part",
					"part_index", i,
					"text_length", len(part.Text))
			case part.FunctionCall != nil:
				toolCalls = append(toolCalls, part.FunctionCall)
				clog.InfoContext(ctx, "Found function call part",
					"part_index", i,
					"function_name", part.FunctionCall.Name,
					"function_id", part.FunctionCall.ID)
			default:
				clog.WarnContext(ctx, "Found part with unexpected content", "part_index", i)
			}
		}

		// If there are tool calls, execute them and send responses
		if len(toolCalls) > 0 {
			// Dispatch the turn's function calls under a bounded pool, holding
			// submit calls out until the pool drains — see
			// execshared.DispatchToolCalls for the concurrency and
			// submit-quiesce semantics. Each handler writes into its own result
			// slot so the shared finalResultPtr is never raced; the response
			// parts are then consumed in the model's original order and the
			// first terminal result (in order) ends the run. Tool handlers must
			// be safe for concurrent use when concurrency exceeds 1.
			type toolOutcome struct {
				committed bool
				err       error
			}
			outcomes := make([]toolOutcome, len(toolCalls))
			perCallResults := make([]Response, len(toolCalls))
			parts := make([]*genai.Part, len(toolCalls))

			execshared.DispatchToolCalls(toolCalls, e.toolCallConcurrency,
				func(call *genai.FunctionCall) bool { return isSubmit(call.Name) },
				func(i int, call *genai.FunctionCall) {
					part, committed, cerr := executeToolCall(call, &perCallResults[i])
					parts[i] = part
					outcomes[i] = toolOutcome{committed: committed, err: cerr}
				})

			var toolResponseParts []*genai.Part
			for i := range toolCalls {
				if outcomes[i].err != nil {
					return zero, nil, true, outcomes[i].err
				}
				// The committed flag is the submit tool's explicit terminal
				// signal — it fires even for a zero-value result, so the model
				// is never told "submitted successfully" on a run that keeps
				// going. The zero-value check preserves the legacy contract
				// for regular tools that write a non-zero result through
				// their result pointer.
				if outcomes[i].committed || !reflect.ValueOf(perCallResults[i]).IsZero() {
					clog.InfoContext(ctx, "Tool set final result, exiting conversation loop", "turns_completed", turn+1)
					e.telemetry.RecordTurns(ctx, turn+1, false)
					return perCallResults[i], nil, true, nil
				}
				toolResponseParts = append(toolResponseParts, parts[i])
			}

			// Send tool responses back to the chat with retry for transient errors
			nextResponse, err := e.sendWithRetry(ctx, turnCfg, "send_tool_responses", "failed to send tool responses", func() (*genai.GenerateContentResponse, error) {
				return chat.Send(ctx, toolResponseParts...)
			})
			if err != nil {
				return zero, nil, true, err
			}
			return zero, nextResponse, false, nil
		}

		// When submit_result is configured, it is the only valid exit path.
		// If the model responds with text instead of calling submit_result,
		// redirect it back to use the tool.
		if e.submitTool.Handler != nil && hasText {
			clog.WarnContext(ctx, "Model responded with text instead of calling submit_result, redirecting")
			e.telemetry.RecordToolCall(ctx, "submit_result_redirect")

			redirectResp, err := e.sendWithRetry(ctx, turnCfg, "send_submit_redirect", "failed to send submit_result redirect", func() (*genai.GenerateContentResponse, error) {
				return chat.SendMessage(ctx, genai.Part{
					Text: "You must call the submit_result tool to return your response. Do not respond with plain text. If you encountered an error or cannot complete the task, call submit_result with an appropriate error or summary.",
				})
			})
			if err != nil {
				return zero, nil, true, err
			}

			return zero, redirectResp, false, nil
		}

		// Fallback: parse text response as JSON when submit_result is not configured
		if hasText {
			extractedResponse, err := result.Extract[Response](responseText)
			if err != nil {
				clog.ErrorContext(ctx, "Failed to parse AI response",
					"response", responseText,
					"error", err)
				return zero, nil, true, fmt.Errorf("failed to parse AI response: %w", err)
			}
			clog.InfoContext(ctx, "Successfully completed Google AI agent execution", "turns_completed", turn+1)
			e.telemetry.RecordTurns(ctx, turn+1, false)
			return extractedResponse, nil, true, nil
		}

		// Unexpected state
		clog.ErrorContext(ctx, "Unexpected response format - no text and no tool calls")
		return zero, nil, true, errors.New("unexpected response format from model")
	}

	for turn := range e.maxTurns {
		result, nextResp, done, err := executeTurn(turn, response)
		// done=true on all terminal paths (including errors); || err != nil is a
		// safety net in case a future path sets err without setting done.
		if done || err != nil {
			return result, err
		}
		if nextResp == nil {
			return resp, errors.New("retry returned nil response")
		}
		response = nextResp
	}

	clog.ErrorContext(ctx, "Agent exceeded maximum conversation turns", "max_turns", e.maxTurns)
	e.telemetry.RecordTurns(ctx, e.maxTurns, true)
	// The final unprocessed response carries real billable tokens (the
	// model already generated them). Without this synthetic turn the
	// loop would exit and only OTel metrics would see them — turns[]
	// is the source of truth for cost analysis (DEV-1140), so leaving
	// them off would silently undercount maxTurns-exhausted runs.
	if response != nil && response.UsageMetadata != nil {
		llmTurn := trace.BeginTurn(e.maxTurns, agenttrace.SystemGoogleVertex, e.model)
		llmTurn.RecordTokens(
			int64(response.UsageMetadata.PromptTokenCount),
			int64(response.UsageMetadata.CandidatesTokenCount),
		)
		if e.cacheControl && response.UsageMetadata.CachedContentTokenCount > 0 {
			llmTurn.RecordCacheTokens(int64(response.UsageMetadata.CachedContentTokenCount), 0)
		}
		llmTurn.End()
	}
	return resp, fmt.Errorf("agent exceeded maximum conversation turns (%d)", e.maxTurns)
}

// submitToolName returns the configured terminal submit tool's name, or ""
// when no submit tool is registered. The default name "submit_result" is used
// when a submit tool is registered without an explicit name.
func (e *executor[Request, Response]) submitToolName() string {
	if e.submitTool.Handler == nil {
		return ""
	}
	if e.submitTool.Definition != nil && e.submitTool.Definition.Name != "" {
		return e.submitTool.Definition.Name
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
	call *genai.FunctionCall,
	trace *agenttrace.Trace[Response],
	resultPtr *Response,
) (map[string]any, bool, error) {
	return execshared.GateSubmission(ctx, e.submitTool.Handler(ctx, call, trace),
		trace, call.ID, call.Name, call.Args,
		e.resultValidators, e.telemetry, e.submitToolName(), resultPtr)
}

// sendWithRetry runs a single chat send under RetryWithBackoff, counts the
// final attempt on the API-request counter, and applies the shared error
// policy: errors from RequeueIfRetryable propagate unwrapped (the workqueue
// requeue path depends on the error type), any other failure is wrapped with
// errContext. On success it records the response's usage metadata as OTel
// token metrics; per-turn token attribution onto the trace happens separately,
// at the top of the next executeTurn iteration.
func (e *executor[Request, Response]) sendWithRetry(
	ctx context.Context,
	cfg retry.RetryConfig,
	operation, errContext string,
	send func() (*genai.GenerateContentResponse, error),
) (*genai.GenerateContentResponse, error) {
	response, err := retry.RetryWithBackoff(ctx, cfg, operation, isRetryableVertexError, send)
	e.telemetry.RecordAPIRequest(ctx, err)
	if err != nil {
		if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableVertexError, "Vertex AI"); requeueErr != nil {
			return nil, requeueErr
		}
		return nil, fmt.Errorf("%s: %w", errContext, err)
	}

	if response != nil && response.UsageMetadata != nil {
		e.recordTokenMetrics(ctx, response.UsageMetadata)
	}
	return response, nil
}

// getOrCreateCache returns the name of a valid CachedContent, creating one if
// needed. It is safe for concurrent use. On cache creation it records
// cache_creation metrics so the cost of the write is visible in dashboards.
func (e *executor[Request, Response]) getOrCreateCache(ctx context.Context, systemInstruction *genai.Content, tools []*genai.Tool) (string, error) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()

	// Cache validity is TTL-only — tool set changes between calls are not detected.
	// Callers must ensure a stable tool set for the lifetime of this executor;
	// use WithoutCacheControl() if the tool set varies per call.
	if e.cachedContentName != "" && time.Now().Add(time.Minute).Before(e.cachedContentExpiry) {
		return e.cachedContentName, nil
	}

	cached, err := e.client.Caches.Create(ctx, e.model, &genai.CreateCachedContentConfig{
		SystemInstruction: systemInstruction,
		Tools:             tools,
		TTL:               e.cacheTTL,
		DisplayName:       fmt.Sprintf("driftlessaf-%s", e.model),
	})
	// Caches.Create is a Vertex API call subject to the same quotas as
	// chat.Send; count it on the same counter so a cache-creation 429 storm
	// shows up on the rate-limit dashboard alongside chat-send 429s.
	e.telemetry.RecordAPIRequest(ctx, err)
	if err != nil {
		return "", fmt.Errorf("creating cached content: %w", err)
	}

	e.cachedContentName = cached.Name
	e.cachedContentExpiry = cached.ExpireTime

	// Record cache creation metrics and log.
	var totalTokenCount int32
	if cached.UsageMetadata != nil {
		totalTokenCount = cached.UsageMetadata.TotalTokenCount
		if totalTokenCount > 0 {
			e.telemetry.RecordCacheTokens(ctx, 0, int64(totalTokenCount))
		}
	}

	clog.InfoContext(ctx, "Created context cache",
		"cache_name", cached.Name,
		"expire_time", cached.ExpireTime,
		"total_token_count", totalTokenCount,
	)

	return cached.Name, nil
}

// ptr is a helper function to create a pointer to a value
func ptr[T any](v T) *T {
	return &v
}

// recordTokenMetrics unpacks the genai usage-metadata shape into the shared
// recorder. When context caching is active and the response includes cached
// tokens, OTel cache metrics are also recorded. Per-turn cache token
// attribution onto the trace happens in executeTurn
// (LLMTurn.RecordCacheTokens) — this path only emits the OTel metrics counter.
func (e *executor[Request, Response]) recordTokenMetrics(ctx context.Context, usage *genai.GenerateContentResponseUsageMetadata) {
	if usage == nil {
		return
	}

	e.telemetry.RecordTokens(ctx, int64(usage.PromptTokenCount), int64(usage.CandidatesTokenCount))

	if e.cacheControl && usage.CachedContentTokenCount > 0 {
		e.telemetry.RecordCacheTokens(ctx, int64(usage.CachedContentTokenCount), 0)
		clog.DebugContext(ctx, "Prompt cache metrics",
			"cache_read_tokens", usage.CachedContentTokenCount)
	}
}
