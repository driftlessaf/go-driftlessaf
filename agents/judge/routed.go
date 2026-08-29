/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// NewRouted constructs a judge from an explicit route selection. It resolves
// one immutable plan, invokes exactly one protocol-typed adapter, and never
// falls back to another provider or the legacy environment-selected path.
func NewRouted(ctx context.Context, router *metaagent.Router, selection modelrouter.Selection) (Interface, error) {
	resolution, err := router.Resolve(selection)
	if err != nil {
		return nil, fmt.Errorf("creating routed judge: %w", err)
	}
	requirements := modelrouter.Requirements{MaximumOutputTokens: true}

	switch resolution.Plan().Protocol() {
	case modelrouter.ProtocolGoogleGenAI:
		binding, err := resolution.BindGoogleGenAI(ctx, requirements)
		if err != nil {
			return nil, fmt.Errorf("creating routed judge: %w", err)
		}
		return newRoutedGoogle(binding)
	case modelrouter.ProtocolAnthropicMessages:
		binding, err := resolution.BindAnthropicMessages(ctx, requirements)
		if err != nil {
			return nil, fmt.Errorf("creating routed judge: %w", err)
		}
		return newRoutedClaude(binding)
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
