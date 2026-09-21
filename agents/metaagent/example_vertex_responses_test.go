/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/metaagent"
)

func ExampleNewVertexOpenAIResponsesAdapter() {
	// Credentials are discovered only when a route binds the service.
	_, err := metaagent.NewVertexOpenAIResponsesAdapter("example-project", "global")
	fmt.Println(err)
	// Output: <nil>
}
