/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
)

// ErrInvalidRouter identifies a nil or incomplete explicit router.
var ErrInvalidRouter = errors.New("invalid model router")

// AdapterRegistries groups the three protocol-fixed adapter registries. A nil
// registry is allowed when an application declares no routes for that
// protocol; selecting one of those routes returns ErrAdapterNotFound.
type AdapterRegistries struct {
	GoogleGenAI           *GoogleGenAIAdapterRegistry
	AnthropicMessages     *AnthropicMessagesAdapterRegistry
	OpenAIChatCompletions *OpenAIChatCompletionsAdapterRegistry
}

// Router combines an immutable route registry with explicitly constructed,
// typed adapter registries. It contains no package-global registration state.
// A Router is safe for concurrent use when its registered adapters are safe
// for concurrent use.
type Router struct {
	routes   *modelrouter.Registry
	adapters AdapterRegistries
}

// RouteResolution carries one route plan together with the Router that
// resolved it. Its private fields preserve plan provenance while allowing
// protocol consumers such as judge to request one typed adapter binding.
// Its exported API exposes no credentials or provider client. A
// RouteResolution is safe to copy and use concurrently; binding concurrently
// requires the selected adapter to be safe for concurrent use.
type RouteResolution struct {
	router *Router
	plan   modelrouter.Plan
}

// NewRouter constructs an explicit meta-agent router.
func NewRouter(routes *modelrouter.Registry, adapters AdapterRegistries) (*Router, error) {
	if routes == nil {
		return nil, fmt.Errorf("%w: route registry is nil", ErrInvalidRouter)
	}
	return &Router{routes: routes, adapters: adapters}, nil
}

// Resolve resolves selection without loading credentials or invoking an
// adapter. The returned RouteResolution can bind only the exact plan produced
// by this Router.
func (r *Router) Resolve(selection modelrouter.Selection) (RouteResolution, error) {
	if r == nil || r.routes == nil {
		return RouteResolution{}, fmt.Errorf("%w: router is nil", ErrInvalidRouter)
	}
	plan, err := r.routes.Resolve(selection)
	if err != nil {
		return RouteResolution{}, fmt.Errorf("resolving model route: %w", err)
	}
	return RouteResolution{router: r, plan: plan}, nil
}

// Plan returns the immutable, secret-free plan carried by r.
func (r RouteResolution) Plan() modelrouter.Plan {
	return r.plan
}

// BindGoogleGenAI validates requirements and invokes the one Google Gen AI
// adapter selected by r.
func (r RouteResolution) BindGoogleGenAI(ctx context.Context, requirements modelrouter.Requirements) (GoogleGenAIBinding, error) {
	if err := r.validate(modelrouter.ProtocolGoogleGenAI, requirements); err != nil {
		return GoogleGenAIBinding{}, err
	}
	adapter, err := r.router.adapters.GoogleGenAI.lookup(r.plan.Provider())
	if err != nil {
		return GoogleGenAIBinding{}, err
	}
	binding, err := adapter(ctx, r.plan)
	if err != nil {
		return GoogleGenAIBinding{}, r.adapterError(err)
	}
	if !binding.initialized {
		return GoogleGenAIBinding{}, fmt.Errorf("%w: adapter returned a zero Google Gen AI binding", ErrInvalidBinding)
	}
	if err := validateReturnedPlan(r.plan, binding.Plan()); err != nil {
		return GoogleGenAIBinding{}, err
	}
	return binding, nil
}

// BindAnthropicMessages validates requirements and invokes the one Anthropic
// Messages adapter selected by r.
func (r RouteResolution) BindAnthropicMessages(ctx context.Context, requirements modelrouter.Requirements) (AnthropicMessagesBinding, error) {
	if err := r.validate(modelrouter.ProtocolAnthropicMessages, requirements); err != nil {
		return AnthropicMessagesBinding{}, err
	}
	adapter, err := r.router.adapters.AnthropicMessages.lookup(r.plan.Provider())
	if err != nil {
		return AnthropicMessagesBinding{}, err
	}
	binding, err := adapter(ctx, r.plan)
	if err != nil {
		return AnthropicMessagesBinding{}, r.adapterError(err)
	}
	if !binding.initialized {
		return AnthropicMessagesBinding{}, fmt.Errorf("%w: adapter returned a zero Anthropic Messages binding", ErrInvalidBinding)
	}
	if err := validateReturnedPlan(r.plan, binding.Plan()); err != nil {
		return AnthropicMessagesBinding{}, err
	}
	return binding, nil
}

func (r RouteResolution) bindOpenAIChatCompletions(ctx context.Context, requirements modelrouter.Requirements) (OpenAIChatCompletionsBinding, error) {
	if err := r.validate(modelrouter.ProtocolOpenAIChatCompletions, requirements); err != nil {
		return OpenAIChatCompletionsBinding{}, err
	}
	adapter, err := r.router.adapters.OpenAIChatCompletions.lookup(r.plan.Provider())
	if err != nil {
		return OpenAIChatCompletionsBinding{}, err
	}
	binding, err := adapter(ctx, r.plan)
	if err != nil {
		return OpenAIChatCompletionsBinding{}, r.adapterError(err)
	}
	if !binding.initialized {
		return OpenAIChatCompletionsBinding{}, fmt.Errorf("%w: adapter returned a zero OpenAI Chat Completions binding", ErrInvalidBinding)
	}
	if err := validateReturnedPlan(r.plan, binding.Plan()); err != nil {
		return OpenAIChatCompletionsBinding{}, err
	}
	return binding, nil
}

func (r RouteResolution) validate(protocol modelrouter.Protocol, requirements modelrouter.Requirements) error {
	if r.router == nil || r.router.routes == nil {
		return fmt.Errorf("%w: route resolution is uninitialized", ErrInvalidRouter)
	}
	if err := r.plan.Validate(); err != nil {
		return fmt.Errorf("%w: route resolution plan: %w", ErrInvalidRouter, err)
	}
	if r.plan.Protocol() != protocol {
		return fmt.Errorf("resolved route protocol is %q, want %q", r.plan.Protocol(), protocol)
	}
	return r.plan.ValidateRequirements(requirements)
}

func (r RouteResolution) adapterError(err error) error {
	return fmt.Errorf("constructing adapter binding for provider %q and protocol %q: %w", r.plan.Provider(), r.plan.Protocol(), err)
}

// NewRouted resolves selection, validates every requested capability, invokes
// exactly one typed adapter, and constructs the matching protocol agent. It
// never falls back to another provider or protocol.
func NewRouted[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	router *Router,
	selection modelrouter.Selection,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	if router == nil || router.routes == nil {
		return nil, fmt.Errorf("%w: router is nil", ErrInvalidRouter)
	}
	if err := validateRoutedConfig(config); err != nil {
		return nil, err
	}

	resolution, err := router.Resolve(selection)
	if err != nil {
		return nil, err
	}
	plan := resolution.Plan()
	requirements := requirementsForConfig(plan.Protocol(), config)
	// Preserve the legacy NewRouted validation order: route capabilities fail
	// before protocol-specific config and submit-schema validation. The typed
	// binder validates again so direct RouteResolution consumers remain safe.
	if err := plan.ValidateRequirements(requirements); err != nil {
		return nil, err
	}
	if err := validateRoutedConfigForProtocol(plan.Protocol(), config); err != nil {
		return nil, err
	}
	// Validate response-schema configuration before an adapter loads
	// credentials or constructs a provider client.
	if err := validateSubmitToolForProtocol(plan.Protocol(), config); err != nil {
		return nil, err
	}

	switch plan.Protocol() {
	case modelrouter.ProtocolGoogleGenAI:
		binding, err := resolution.BindGoogleGenAI(ctx, requirements)
		if err != nil {
			return nil, err
		}
		return newRoutedGoogleAgent[Req, Resp, CB](binding, config)

	case modelrouter.ProtocolAnthropicMessages:
		binding, err := resolution.BindAnthropicMessages(ctx, requirements)
		if err != nil {
			return nil, err
		}
		return newRoutedClaudeAgent[Req, Resp, CB](binding, config)

	case modelrouter.ProtocolOpenAIChatCompletions:
		binding, err := resolution.bindOpenAIChatCompletions(ctx, requirements)
		if err != nil {
			return nil, err
		}
		return newRoutedOpenAIChatCompletionsAgent[Req, Resp, CB](binding, config)

	default:
		// Registry resolution already validates the controlled protocol set. Keep
		// this guard so a corrupted Router never reaches an adapter.
		return nil, fmt.Errorf("%w: unsupported resolved protocol %q", ErrInvalidRouter, plan.Protocol())
	}
}

func requirementsForConfig[Resp, CB any](protocol modelrouter.Protocol, config Config[Resp, CB]) modelrouter.Requirements {
	return modelrouter.Requirements{
		Effort:                 config.Effort,
		ExplicitThinkingBudget: config.ThinkingBudget != 0,
		// UserPromptSuffix requests an explicit cache boundary only on the
		// Anthropic Messages path. Google and OpenAI preserve their documented
		// concatenation behavior without claiming prompt-cache semantics.
		PromptCaching:       config.UserPromptSuffix != nil && protocol == modelrouter.ProtocolAnthropicMessages,
		ToolCalling:         true,
		TerminalSubmission:  true,
		SuspendResume:       config.SuspendToolName != "",
		MaximumOutputTokens: config.MaxTokens != 0,
		RefusalRecovery:     config.RefusalNudgeMaxRetries != 0,
	}
}

func validateRoutedConfig[Resp, CB any](config Config[Resp, CB]) error {
	if config.UserPrompt == nil {
		return errors.New("creating routed meta-agent: prompt cannot be nil")
	}
	if config.Tools == nil {
		return errors.New("creating routed meta-agent: tools provider cannot be nil")
	}
	if config.MaxTurns < 0 {
		return fmt.Errorf("creating routed meta-agent: max turns must not be negative, got %d", config.MaxTurns)
	}
	if config.ToolCallConcurrency < 0 {
		return fmt.Errorf("creating routed meta-agent: tool call concurrency must not be negative, got %d", config.ToolCallConcurrency)
	}
	if config.MaxTokens < 0 {
		return fmt.Errorf("creating routed meta-agent: max tokens must not be negative, got %d", config.MaxTokens)
	}
	if config.ThinkingBudget < 0 {
		return fmt.Errorf("creating routed meta-agent: thinking budget must not be negative, got %d", config.ThinkingBudget)
	}
	if config.ThinkingBudget != 0 && config.Effort != "" {
		return errors.New("creating routed meta-agent: ThinkingBudget and Effort are mutually exclusive")
	}
	if config.RefusalNudgeMaxRetries < 0 {
		return fmt.Errorf("creating routed meta-agent: refusal nudge max retries must not be negative, got %d", config.RefusalNudgeMaxRetries)
	}
	for i, validator := range config.ResultValidators {
		if validator == nil {
			return fmt.Errorf("creating routed meta-agent: result validator at index %d is nil", i)
		}
	}
	return nil
}

func validateRoutedConfigForProtocol[Resp, CB any](protocol modelrouter.Protocol, config Config[Resp, CB]) error {
	switch protocol {
	case modelrouter.ProtocolGoogleGenAI:
		if config.MaxTokens > 65536 {
			return fmt.Errorf("creating routed Google executor: max output tokens %d exceeds maximum of 65536", config.MaxTokens)
		}
		if config.ThinkingBudget > math.MaxInt32 {
			return fmt.Errorf("creating routed Google executor: thinking budget %d exceeds maximum integer value of %d", config.ThinkingBudget, int64(math.MaxInt32))
		}
		maxOutputTokens := cmp.Or(config.MaxTokens, int64(65536))
		if config.ThinkingBudget > 0 && config.ThinkingBudget >= maxOutputTokens {
			return fmt.Errorf("creating routed Google executor: thinking budget (%d) must be less than max output tokens (%d)", config.ThinkingBudget, maxOutputTokens)
		}

	case modelrouter.ProtocolAnthropicMessages:
		maxTokens := cmp.Or(config.MaxTokens, defaultMaxTokens)
		if maxTokens > 128000 {
			return fmt.Errorf("creating routed Claude executor: max tokens %d exceeds maximum of 128000", maxTokens)
		}
		if config.ThinkingBudget > 0 && config.ThinkingBudget < 1024 {
			return fmt.Errorf("creating routed Claude executor: thinking budget_tokens must be at least 1024, got %d", config.ThinkingBudget)
		}
		if config.ThinkingBudget > 0 && config.ThinkingBudget >= maxTokens {
			return fmt.Errorf("creating routed Claude executor: thinking budget_tokens (%d) must be less than max_tokens (%d)", config.ThinkingBudget, maxTokens)
		}

	case modelrouter.ProtocolOpenAIChatCompletions:
		// The OpenAI executor accepts every positive int64 output limit. Other
		// unsupported features were rejected by Plan.ValidateRequirements.
	}
	return nil
}

func validateSubmitToolForProtocol[Resp, CB any](protocol modelrouter.Protocol, config Config[Resp, CB]) error {
	var err error
	switch protocol {
	case modelrouter.ProtocolGoogleGenAI:
		_, err = submitresult.GoogleTool(submitOptions(config))
	case modelrouter.ProtocolAnthropicMessages:
		_, err = submitresult.ClaudeTool(submitOptions(config))
	case modelrouter.ProtocolOpenAIChatCompletions:
		_, err = submitresult.OpenAITool(submitOptions(config))
	}
	if err != nil {
		return fmt.Errorf("building submit tool: %w", err)
	}
	return nil
}

func routedAttribution(plan modelrouter.Plan) agenttrace.Attribution {
	attribution := plan.Attribution()
	return agenttrace.Attribution{
		ProviderName: attribution.ProviderName,
		System:       attribution.LegacySystem,
		LogicalModel: plan.LogicalModel(),
		Protocol:     string(plan.Protocol()),
	}
}
