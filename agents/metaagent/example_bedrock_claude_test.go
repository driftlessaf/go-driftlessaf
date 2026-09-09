/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/metaagent"
)

func ExampleNewBedrockRuntimeAnthropicMessagesAdapter() {
	adapter, err := metaagent.NewBedrockRuntimeAnthropicMessagesAdapter(awsauth.Config{
		Region:  "us-west-2",
		Profile: "approved-sso-profile",
	})
	fmt.Println(adapter != nil, err)
	// Construction does not load credentials or invoke a model. Register this
	// adapter for ProviderAWSBedrock in the Anthropic Messages registry, using
	// an application-owned route with the exact approved inference profile ID.
	// Output: true <nil>
}
