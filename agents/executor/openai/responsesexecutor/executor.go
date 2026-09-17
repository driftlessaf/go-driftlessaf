/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/schema"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

// Config configures a credential-free Responses executor. Zero numeric limits
// select defaults: 200 turns, 32768 output tokens, and 10 concurrent tools.
// Concurrent tools must synchronize shared state themselves. Reasoning and cache
// tokens are subsets of output and input tokens, respectively, not extra usage.
type Config[Response any] struct {
	Model               string
	Attribution         agenttrace.Attribution
	UserPrompt          *promptbuilder.Prompt
	SystemInstructions  *promptbuilder.Prompt
	UserPromptSuffix    *promptbuilder.Prompt
	MaxTurns            int
	ToolCallConcurrency int
	MaxTokens           int64
	// Effort is sent verbatim. Routed callers validate model support against
	// the route's capabilities; direct callers must select a supported level.
	Effort           effort.Level
	Submit           submitresult.Options[Response]
	ResultValidators []callbacks.ResultValidator[Response]
	ResourceLabels   map[string]string

	// RequestTimeout bounds each streaming HTTP attempt. Zero inherits the
	// execution deadline. This is a total timeout, not an idle timeout.
	RequestTimeout time.Duration

	// ExecutionTimeout bounds the conversation, including tools and retries.
	// Zero selects 30 minutes. A shorter caller deadline always wins.
	ExecutionTimeout time.Duration
}

// Interface executes a native Responses conversation with provider-neutral tools.
type Interface[Request promptbuilder.Bindable, Response any] interface {
	Execute(context.Context, Request, map[string]toolcall.Tool[Response]) (Response, error)
}

type executor[Request promptbuilder.Bindable, Response any] struct {
	client   responses.ResponseService
	config   Config[Response]
	submit   submitresult.ResponsesMetadata[Response]
	recorder *telemetry.Recorder
}

// Empty requested means provider default; empty reported means unavailable.
type reasoningEffortEvidence struct {
	Turn      int          `json:"turn"`
	Requested effort.Level `json:"requested"`
	Reported  effort.Level `json:"reported"`
}

// New constructs a native Responses executor using an already configured client.
// It does not access the network or inspect ambient authentication variables.
func New[Request promptbuilder.Bindable, Response any](client responses.ResponseService, cfg Config[Response]) (Interface[Request, Response], error) {
	if cfg.UserPrompt == nil || strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("responses requires a prompt and model")
	}
	if err := cfg.Attribution.Validate(); err != nil {
		return nil, fmt.Errorf("responses attribution: %w", err)
	}
	if cfg.Attribution.Protocol != "openai-responses" {
		return nil, errors.New("responses requires openai-responses attribution")
	}
	if cfg.MaxTurns < 0 || cfg.ToolCallConcurrency < 0 || cfg.MaxTokens < 0 {
		return nil, errors.New("responses limits cannot be negative")
	}
	if cfg.RequestTimeout < 0 || cfg.ExecutionTimeout < 0 {
		return nil, errors.New("responses timeouts cannot be negative")
	}
	if cfg.Effort != "" {
		if err := cfg.Effort.Validate(); err != nil {
			return nil, err
		}
	}
	for _, v := range cfg.ResultValidators {
		if v == nil {
			return nil, errors.New("responses result validator cannot be nil")
		}
	}
	cfg.MaxTurns = cmp.Or(cfg.MaxTurns, 200)
	cfg.MaxTokens = cmp.Or(cfg.MaxTokens, int64(32768))
	cfg.ToolCallConcurrency = cmp.Or(cfg.ToolCallConcurrency, 10)
	cfg.ExecutionTimeout = cmp.Or(cfg.ExecutionTimeout, 30*time.Minute)
	cfg.ResultValidators = append([]callbacks.ResultValidator[Response]{schema.ResultValidator[Response]()}, cfg.ResultValidators...)
	cfg.ResourceLabels = execshared.DefaultResourceLabels(cfg.ResourceLabels)
	submit, err := submitresult.ResponsesTool(cfg.Submit)
	if err != nil {
		return nil, err
	}
	return &executor[Request, Response]{
		client:   client,
		config:   cfg,
		submit:   submit,
		recorder: telemetry.NewRecorder(metrics.NewGenAI("chainguard.ai.agents"), cfg.Model, cfg.Attribution.ProviderName, cfg.ResourceLabels, statusCode),
	}, nil
}

func (e *executor[Request, Response]) Execute(ctx context.Context, request Request, tools map[string]toolcall.Tool[Response]) (response Response, err error) {
	// A caller's shorter deadline still wins. A provider that stops sending
	// events cannot leave a run alive indefinitely.
	ctx, cancel := context.WithTimeout(ctx, e.config.ExecutionTimeout)
	defer cancel()
	bound, err := request.Bind(e.config.UserPrompt)
	if err != nil {
		return response, err
	}
	prompt, err := bound.Build()
	if err != nil {
		return response, err
	}
	prompt, err = execshared.AppendUserPromptSuffix(prompt, e.config.UserPromptSuffix)
	if err != nil {
		return response, err
	}
	trace, done := agenttrace.StartTrace[Response](ctx, prompt)
	defer func() { done(response, err) }()
	// Structural evidence survives payload truncation and does not contain
	// prompts or reasoning text. Write only after all tool workers have joined.
	var reasoningEvidence []reasoningEffortEvidence
	defer func() { trace.Metadata["responses_reasoning_effort"] = reasoningEvidence }()
	defs, err := e.definitions(tools)
	if err != nil {
		return response, err
	}
	params := responses.ResponseNewParams{
		Model:             e.config.Model,
		Store:             param.NewOpt(false),
		MaxOutputTokens:   param.NewOpt(e.config.MaxTokens),
		ParallelToolCalls: param.NewOpt(true),
		Include:           []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		Tools:             defs,
		Input:             responses.ResponseNewParamsInputUnion{OfInputItemList: responses.ResponseInputParam{responses.ResponseInputItemParamOfMessage(prompt, "user")}},
	}
	if e.config.SystemInstructions != nil {
		instructions, err := e.config.SystemInstructions.Build()
		if err != nil {
			return response, err
		}
		params.Instructions = param.NewOpt(instructions)
	}
	if e.config.Effort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(e.config.Effort)}
	}
	invalid := 0
	turns := 0
	limitExceeded := false
	defer func() { e.recorder.RecordTurns(ctx, turns, limitExceeded) }()
	seenCalls := make(map[string]struct{})
	for index := range e.config.MaxTurns {
		if err := ctx.Err(); err != nil {
			return response, err
		}
		turn := trace.BeginTurnWithAttribution(index, e.config.Model, e.config.Attribution)
		turns++
		accepted, usable, turnErr := func() (bool, bool, error) {
			defer turn.End()
			requestPayload, err := safePayload(params)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			if err := turn.RecordRequest(requestPayload); err != nil {
				turn.Fail(err)
				return false, false, err
			}
			rc := retry.RetryConfig{MaxRetries: 2, BaseBackoff: time.Second, MaxBackoff: 4 * time.Second, OnAttemptError: turn.RecordError}
			out, err := retry.RetryWithBackoff(ctx, rc, "responses request", retryable, func() (*responses.Response, error) {
				out, err := e.stream(ctx, params)
				e.recorder.RecordAPIRequest(ctx, err)
				return out, err
			})
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			turn.RecordTokens(out.Usage.InputTokens, out.Usage.OutputTokens)
			turn.RecordCacheTokens(out.Usage.InputTokensDetails.CachedTokens, 0)
			turn.RecordReasoningTokens(out.Usage.OutputTokensDetails.ReasoningTokens)
			reported := effort.Level(out.Reasoning.Effort)
			switch reported {
			case "", "none", "minimal", effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max:
			default:
				reported = ""
			}
			reasoningEvidence = append(reasoningEvidence, reasoningEffortEvidence{
				Turn: index, Requested: e.config.Effort, Reported: reported,
			})
			e.recorder.RecordTokens(ctx, out.Usage.InputTokens, out.Usage.OutputTokens)
			e.recorder.RecordCacheTokens(ctx, out.Usage.InputTokensDetails.CachedTokens, 0)
			responsePayload, err := safePayload(out)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			if err := turn.RecordResponse(responsePayload); err != nil {
				turn.Fail(err)
				return false, false, err
			}
			if e.config.Effort != "" && reported != "" && reported != e.config.Effort {
				err := fmt.Errorf("responses reasoning effort mismatch: requested %q, provider reported %q", e.config.Effort, reported)
				turn.Fail(err)
				return false, false, err
			}
			// Preserve native messages, calls, and opaque reasoning. Do not put
			// encrypted reasoning or raw provider payloads in logs or traces.
			items, calls, err := continuation(out, seenCalls)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			params.Input.OfInputItemList = append(params.Input.OfInputItemList, items...)
			accepted, usable, outputs, err := e.dispatch(ctx, calls, tools, trace, &response)
			if err != nil {
				turn.Fail(err)
				return false, false, err
			}
			params.Input.OfInputItemList = append(params.Input.OfInputItemList, outputs...)
			if len(calls) == 0 {
				params.Input.OfInputItemList = append(params.Input.OfInputItemList, responses.ResponseInputItemParamOfMessage("Continue using the available tools, then submit the complete result with the terminal tool.", "user"))
			}
			return accepted, usable, nil
		}()
		if turnErr != nil {
			if requeue := retry.RequeueIfRetryable(ctx, turnErr, retryable, e.config.Attribution.ProviderName); requeue != nil {
				return response, requeue
			}
			return response, turnErr
		}
		if accepted {
			return response, nil
		}
		if usable {
			invalid = 0
		} else {
			invalid++
		}
		if invalid >= 3 {
			return response, errors.New("responses exceeded three consecutive unusable turns")
		}
	}
	limitExceeded = true
	return response, errors.New("responses exceeded maximum turns without an accepted submission")
}

func (e *executor[Request, Response]) definitions(tools map[string]toolcall.Tool[Response]) ([]responses.ToolUnionParam, error) {
	defs := []responses.ToolUnionParam{{OfFunction: &e.submit.Definition}}
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		t := tools[name]
		if name == e.submit.Definition.Name || name == "" || name != t.Def.Name || t.Handler == nil {
			return nil, errors.New("responses tool has an invalid name, handler, or terminal-name collision")
		}
		properties := map[string]any{"reasoning": map[string]any{"type": "string", "description": "Why this tool is needed."}}
		required := []string{"reasoning"}
		for _, p := range t.Def.Parameters {
			properties[p.Name] = toolcall.ParameterToMap(p)
			if p.Required && p.Name != "reasoning" {
				required = append(required, p.Name)
			}
		}
		input := maps.Clone(t.Def.InputSchemaExtensions)
		if input == nil {
			input = make(map[string]any)
		}
		input["type"], input["properties"], input["required"] = "object", properties, required
		if t.Def.InputSchemaDescription != "" {
			input["description"] = t.Def.InputSchemaDescription
		}
		if len(t.Def.InputSchemaDefs) != 0 {
			input["$defs"] = toolcall.SchemaToMap(&toolcall.Schema{Defs: t.Def.InputSchemaDefs})["$defs"]
		}
		defs = append(defs, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{Name: name, Description: param.NewOpt(t.Def.Description), Parameters: input, Strict: param.NewOpt(false)}})
	}
	return defs, nil
}

func continuation(out *responses.Response, seen map[string]struct{}) (responses.ResponseInputParam, []responses.ResponseFunctionToolCall, error) {
	var items responses.ResponseInputParam
	var calls []responses.ResponseFunctionToolCall
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type != "output_text" && c.Type != "refusal" {
					return nil, nil, errors.New("responses returned unsupported message content")
				}
			}
			items = append(items, responses.ResponseInputItemUnionParam{OfOutputMessage: new(item.AsMessage().ToParam())})
		case "reasoning":
			items = append(items, responses.ResponseInputItemUnionParam{OfReasoning: new(item.AsReasoning().ToParam())})
		case "function_call":
			call := item.AsFunctionCall()
			_, repeated := seen[call.CallID]
			if call.CallID == "" || call.Name == "" || repeated || len(calls) >= 128 {
				return nil, nil, errors.New("responses returned invalid or repeated function call identifiers")
			}
			seen[call.CallID] = struct{}{}
			calls = append(calls, call)
			items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCall: new(call.ToParam())})
		default:
			return nil, nil, errors.New("responses returned unsupported output item")
		}
	}
	return items, calls, nil
}

func (e *executor[Request, Response]) dispatch(ctx context.Context, calls []responses.ResponseFunctionToolCall, tools map[string]toolcall.Tool[Response], trace *agenttrace.Trace[Response], result *Response) (bool, bool, responses.ResponseInputParam, error) {
	type outcome struct {
		output           map[string]any
		accepted, usable bool
		err              error
	}
	outcomes := make([]outcome, len(calls))
	isSubmit := execshared.SubmitPredicate(tools, e.submit.Definition.Name, true)
	committed := false // terminal callbacks run sequentially after ordinary tools join
	execshared.DispatchToolCalls(calls, e.config.ToolCallConcurrency, func(c responses.ResponseFunctionToolCall) bool { return isSubmit(c.Name) }, func(i int, c responses.ResponseFunctionToolCall) {
		o := &outcomes[i]
		if err := ctx.Err(); err != nil {
			o.err = err
			return
		}
		var args map[string]any
		if json.Unmarshal([]byte(c.Arguments), &args) != nil || args == nil {
			err := errors.New("tool arguments must be a JSON object")
			trace.RejectedToolCall(c.CallID, c.Name, nil, err)
			o.output = map[string]any{"error": err.Error()}
			return
		}
		call := toolcall.ToolCall{ID: c.CallID, Name: c.Name, Args: args}
		e.recorder.RecordToolCall(ctx, c.Name)
		if isSubmit(c.Name) {
			if committed {
				o.output = map[string]any{"error": "a terminal result was already accepted"}
				return
			}
			submission := e.submit.Handler(ctx, call, trace)
			o.output, o.accepted, o.err = execshared.GateSubmission(ctx, submission, trace, c.CallID, c.Name, args, e.config.ResultValidators, e.recorder, e.submit.Definition.Name, result)
			committed = o.accepted
		} else if t, ok := tools[c.Name]; ok {
			// A regular handler cannot write the shared final result. The
			// terminal validator is the only authority to commit it.
			var scratch Response
			o.output = t.Handler(ctx, call, trace, &scratch)
			o.usable = true
		} else {
			trace.RejectedToolCall(c.CallID, c.Name, args, errors.New("unknown tool"))
			o.output = map[string]any{"error": "unknown tool; use an advertised function"}
		}
	})
	var outputs responses.ResponseInputParam
	usable, accepted := false, false
	for i, o := range outcomes {
		if o.err != nil {
			return false, false, nil, o.err
		}
		accepted = accepted || o.accepted
		usable = usable || o.usable
		b, err := json.Marshal(o.output)
		if err != nil {
			return false, false, nil, errors.New("responses tool result is not JSON encodable")
		}
		if len(b) > maxPayloadBytes {
			return false, false, nil, errors.New("responses tool result exceeds byte limit")
		}
		outputs = append(outputs, responses.ResponseInputItemParamOfFunctionCallOutput(calls[i].CallID, string(b)))
	}
	return accepted, usable, outputs, nil
}
