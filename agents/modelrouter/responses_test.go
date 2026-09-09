/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter_test

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func TestNativeResponsesRouteCapabilities(t *testing.T) {
	t.Parallel()
	route := newRoute(modelrouter.ProviderAWSBedrock, "gpt-5.6-sol", modelrouter.ProtocolOpenAIResponses, "us.openai.gpt-5.6-sol")
	route.Capabilities = modelrouter.Capabilities{ToolCalling: true, TerminalSubmission: true, MaximumOutputTokens: true, SamplingParameters: true, PromptCaching: true, SuspendResume: true, RefusalRecovery: true, ExplicitThinkingBudget: true}
	r, err := modelrouter.NewRegistry(route)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Resolve(route.Selection)
	if err != nil {
		t.Fatal(err)
	}
	c := plan.Capabilities()
	if !c.ToolCalling || !c.TerminalSubmission || !c.MaximumOutputTokens || c.SamplingParameters || c.PromptCaching || c.SuspendResume || c.RefusalRecovery || c.ExplicitThinkingBudget {
		t.Fatalf("capabilities=%+v", c)
	}
	for _, model := range []string{"claude-sonnet-5", "gemini-3.5-flash"} {
		route.Selection.LogicalModel = model
		if _, err := modelrouter.NewRegistry(route); !errors.Is(err, modelrouter.ErrInvalidRoute) {
			t.Errorf("%s: %v", model, err)
		}
	}
}
