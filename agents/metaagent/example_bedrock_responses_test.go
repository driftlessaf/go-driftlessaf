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

func ExampleNewBedrockOpenAIResponsesAdapter() {
	// Omit Profile in workloads using the approved web-identity configuration.
	// Construction validates configuration without discovering credentials.
	_, err := metaagent.NewBedrockOpenAIResponsesAdapter(awsauth.Config{Region: "us-west-2", Profile: "approved-sso-profile"})
	fmt.Println(err)
	// Output: <nil>
}
