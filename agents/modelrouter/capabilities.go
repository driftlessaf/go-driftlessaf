/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter

import (
	"fmt"
	"slices"
	"strings"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/model"
)

// Capabilities is an explicit allow-set of route features. The zero value
// supports no optional features, so adding a field fails closed until a route
// declares it. Nil and empty Efforts both mean that effort isn't supported.
// The Efforts slice returned by a Plan is a copy and can be modified by the
// caller.
type Capabilities struct {
	// Efforts lists the provider-neutral effort levels the route accepts.
	Efforts []effort.Level
	// ExplicitThinkingBudget reports whether the route accepts an explicit
	// token budget instead of provider-neutral effort.
	ExplicitThinkingBudget bool
	// SamplingParameters reports whether the route accepts caller-configured
	// temperature, top-p, or top-k values.
	SamplingParameters bool
	// PromptCaching reports whether the executor implements explicit prompt
	// cache-boundary semantics. Merely concatenating a prompt suffix or relying
	// on provider-managed implicit caching doesn't satisfy this capability.
	PromptCaching bool
	// ToolCalling reports whether the executor implements nonterminal tools.
	ToolCalling bool
	// TerminalSubmission reports whether the executor implements the terminal
	// submit-result tool.
	TerminalSubmission bool
	// SuspendResume reports whether the executor can suspend and later resume a
	// conversation.
	SuspendResume bool
	// MaximumOutputTokens reports whether the executor accepts an explicit
	// output-token limit.
	MaximumOutputTokens bool
	// RefusalRecovery reports whether the executor implements refusal nudges.
	RefusalRecovery bool
}

// SupportsEffort reports whether c supports level.
func (c Capabilities) SupportsEffort(level effort.Level) bool {
	return slices.Contains(c.Efforts, level)
}

// Requirements describes application features that a route must support.
// The zero value requests no optional capability. An empty Effort leaves the
// provider's model default in place.
type Requirements struct {
	Effort                 effort.Level
	ExplicitThinkingBudget bool
	SamplingParameters     bool
	PromptCaching          bool
	ToolCalling            bool
	TerminalSubmission     bool
	SuspendResume          bool
	MaximumOutputTokens    bool
	RefusalRecovery        bool
}

func protocolCapabilities(protocol Protocol) Capabilities {
	allEfforts := []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}
	common := Capabilities{
		Efforts:             allEfforts,
		SamplingParameters:  true,
		ToolCalling:         true,
		TerminalSubmission:  true,
		MaximumOutputTokens: true,
	}

	switch protocol {
	case ProtocolGoogleGenAI:
		common.ExplicitThinkingBudget = true
		return common
	case ProtocolAnthropicMessages:
		common.ExplicitThinkingBudget = true
		common.PromptCaching = true
		common.SuspendResume = true
		common.RefusalRecovery = true
		return common
	case ProtocolOpenAIChatCompletions:
		return common
	default:
		return Capabilities{}
	}
}

func composeCapabilities(protocol Protocol, info model.Info, declared Capabilities) Capabilities {
	effective := protocolCapabilities(protocol)
	effective.Efforts = intersectEfforts(effective.Efforts, info.Efforts)
	effective.SamplingParameters = effective.SamplingParameters && info.SamplingParams
	effective.ExplicitThinkingBudget = effective.ExplicitThinkingBudget &&
		(info.ExtendedThinkingBudget || info.ThinkingControl == model.ThinkingControlBudget)

	effective.Efforts = intersectEfforts(effective.Efforts, declared.Efforts)
	effective.ExplicitThinkingBudget = effective.ExplicitThinkingBudget && declared.ExplicitThinkingBudget
	effective.SamplingParameters = effective.SamplingParameters && declared.SamplingParameters
	effective.PromptCaching = effective.PromptCaching && declared.PromptCaching
	effective.ToolCalling = effective.ToolCalling && declared.ToolCalling
	effective.TerminalSubmission = effective.TerminalSubmission && declared.TerminalSubmission
	effective.SuspendResume = effective.SuspendResume && declared.SuspendResume
	effective.MaximumOutputTokens = effective.MaximumOutputTokens && declared.MaximumOutputTokens
	effective.RefusalRecovery = effective.RefusalRecovery && declared.RefusalRecovery
	return effective
}

func validateCapabilities(capabilities Capabilities) error {
	seen := make(map[effort.Level]struct{}, len(capabilities.Efforts))
	for i, level := range capabilities.Efforts {
		if err := level.Validate(); err != nil {
			return fmt.Errorf("effort at index %d: %w", i, err)
		}
		if _, ok := seen[level]; ok {
			return fmt.Errorf("duplicate effort %q", level)
		}
		seen[level] = struct{}{}
	}
	return nil
}

func validateRequirements(capabilities Capabilities, requirements Requirements) error {
	var unsupported []string
	if requirements.Effort != "" {
		if err := requirements.Effort.Validate(); err != nil {
			return fmt.Errorf("invalid capability requirement: %w", err)
		}
		if !capabilities.SupportsEffort(requirements.Effort) {
			unsupported = append(unsupported, fmt.Sprintf("effort %q", requirements.Effort))
		}
	}
	if requirements.ExplicitThinkingBudget && !capabilities.ExplicitThinkingBudget {
		unsupported = append(unsupported, "explicit thinking budget")
	}
	if requirements.SamplingParameters && !capabilities.SamplingParameters {
		unsupported = append(unsupported, "sampling parameters")
	}
	if requirements.PromptCaching && !capabilities.PromptCaching {
		unsupported = append(unsupported, "prompt caching")
	}
	if requirements.ToolCalling && !capabilities.ToolCalling {
		unsupported = append(unsupported, "tool calling")
	}
	if requirements.TerminalSubmission && !capabilities.TerminalSubmission {
		unsupported = append(unsupported, "terminal submission")
	}
	if requirements.SuspendResume && !capabilities.SuspendResume {
		unsupported = append(unsupported, "suspend and resume")
	}
	if requirements.MaximumOutputTokens && !capabilities.MaximumOutputTokens {
		unsupported = append(unsupported, "maximum output tokens")
	}
	if requirements.RefusalRecovery && !capabilities.RefusalRecovery {
		unsupported = append(unsupported, "refusal recovery")
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("%w: %s", ErrUnsupportedCapability, strings.Join(unsupported, ", "))
	}
	return nil
}

func intersectEfforts(left, right []effort.Level) []effort.Level {
	intersection := make([]effort.Level, 0, len(left))
	for _, level := range left {
		if slices.Contains(right, level) {
			intersection = append(intersection, level)
		}
	}
	return intersection
}

func cloneCapabilities(capabilities Capabilities) Capabilities {
	capabilities.Efforts = slices.Clone(capabilities.Efforts)
	return capabilities
}
