/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package model_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/model"
)

func ExampleResolve() {
	info := model.Resolve("claude-fable-5")
	fmt.Println(info.Backend)
	fmt.Println(info.SamplingParams)
	fmt.Println(info.Efforts)

	info = model.Resolve("gemini-3-pro-preview")
	fmt.Println(info.Backend)
	fmt.Println(info.ThinkingControl)
	// Output:
	// claude
	// false
	// [low medium high xhigh max]
	// gemini
	// level
}

func ExampleInfo_SupportsEffort() {
	fmt.Println(model.Resolve("claude-opus-4-6").SupportsEffort(effort.XHigh))
	fmt.Println(model.Resolve("claude-fable-5").SupportsEffort(effort.XHigh))
	// Output:
	// false
	// true
}
