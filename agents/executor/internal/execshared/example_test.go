/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

func ExampleAppendUserPromptSuffix() {
	suffix, err := promptbuilder.NewPrompt("Focus on error handling.")
	if err != nil {
		panic(err)
	}
	prompt, err := execshared.AppendUserPromptSuffix("changeset payload", suffix)
	if err != nil {
		panic(err)
	}
	fmt.Println(prompt)
	// Output:
	// changeset payload
	//
	// Focus on error handling.
}

func ExampleDefaultResourceLabels() {
	// Custom labels override the environment-derived defaults on key match.
	labels := execshared.DefaultResourceLabels(map[string]string{"team": "platform"})
	fmt.Println(labels["team"])
	// Output:
	// platform
}
