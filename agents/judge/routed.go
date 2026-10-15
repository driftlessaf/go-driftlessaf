/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"cmp"
	"context"
	"fmt"
	"math"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// defaultMaxTokens caps each judgement's output when the target sets no cap.
const defaultMaxTokens int64 = 8192

// NewWithTarget constructs a judge from a startup-owned target. It delegates to
// NewRouted, preserving judge-specific validation and selected-adapter errors.
// The target's cap, when set, replaces the judge's 8,192-token default.
func NewWithTarget(ctx context.Context, target metaagent.Target) (Interface, error) {
	router, selection, err := target.Resolve()
	if err != nil {
		return nil, err
	}
	return newRouted(ctx, router, selection, cmp.Or(target.MaxTokens(), defaultMaxTokens))
}

// NewRouted constructs a judge from an explicit route selection. It resolves
// one immutable plan, invokes exactly one protocol-typed adapter, and never
// falls back to another provider or the legacy environment-selected path.
func NewRouted(ctx context.Context, router *metaagent.Router, selection modelrouter.Selection) (Interface, error) {
	return newRouted(ctx, router, selection, defaultMaxTokens)
}

func newRouted(ctx context.Context, router *metaagent.Router, selection modelrouter.Selection, maxTokens int64) (Interface, error) {
	resolution, err := router.Resolve(selection)
	if err != nil {
		return nil, fmt.Errorf("creating routed judge: %w", err)
	}
	requirements := modelrouter.Requirements{MaximumOutputTokens: true}

	switch resolution.Plan().Protocol() {
	case modelrouter.ProtocolGoogleGenAI:
		if maxTokens > math.MaxInt32 {
			return nil, fmt.Errorf("creating routed judge: max output tokens %d exceeds maximum of %d", maxTokens, math.MaxInt32)
		}
		binding, err := resolution.BindGoogleGenAI(ctx, requirements)
		if err != nil {
			return nil, fmt.Errorf("creating routed judge: %w", err)
		}
		return newRoutedGoogle(binding, int32(maxTokens))
	case modelrouter.ProtocolAnthropicMessages:
		binding, err := resolution.BindAnthropicMessages(ctx, requirements)
		if err != nil {
			return nil, fmt.Errorf("creating routed judge: %w", err)
		}
		return newRoutedClaude(binding, maxTokens)
	default:
		return nil, fmt.Errorf("creating routed judge: protocol %q is not supported", resolution.Plan().Protocol())
	}
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
