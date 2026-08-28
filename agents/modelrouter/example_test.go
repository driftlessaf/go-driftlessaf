/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func Example() {
	selection := modelrouter.Selection{
		Provider:     modelrouter.ProviderAWSBedrock,
		LogicalModel: "claude-sonnet-5",
	}
	registry, err := modelrouter.NewRegistry(modelrouter.Route{
		Selection:       selection,
		Protocol:        modelrouter.ProtocolAnthropicMessages,
		ProviderModelID: "anthropic.claude-sonnet-5",
		Capabilities: modelrouter.Capabilities{
			Efforts:            []effort.Level{effort.High},
			ToolCalling:        true,
			TerminalSubmission: true,
		},
	})
	if err != nil {
		panic(err)
	}

	plan, err := registry.Resolve(selection)
	if err != nil {
		panic(err)
	}
	if err := plan.ValidateRequirements(modelrouter.Requirements{
		Effort:             effort.High,
		ToolCalling:        true,
		TerminalSubmission: true,
	}); err != nil {
		panic(err)
	}

	fmt.Printf("%s uses %s through %s\n", plan.LogicalModel(), plan.Provider(), plan.Protocol())
	// Output:
	// claude-sonnet-5 uses bedrock through anthropic-messages
}
