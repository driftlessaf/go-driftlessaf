/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package anthropicauth_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
)

// ExampleConfig_Configured demonstrates how backend selection is keyed off the
// federation rule ID and the identity-token file: both must be set to select
// the Anthropic-direct backend; anything less keeps the Vertex default.
func ExampleConfig_Configured() {
	fmt.Println(anthropicauth.Config{}.Configured())
	fmt.Println(anthropicauth.Config{
		FederationRuleID:  "fdrl_0123456789",
		IdentityTokenFile: "/run/oidc/token",
	}.Configured())
	// Output:
	// false
	// true
}

// ExampleNewClient demonstrates constructing a Claude client with an explicit
// Config. An empty Config selects Vertex AI; a Config with the federation
// fields selects the Anthropic-direct first-party API + WIF backend.
// Entrypoints that want env-driven selection pass ConfigFromEnv() instead.
func ExampleNewClient() {
	ctx := context.Background()

	client := anthropicauth.NewClient(ctx, "my-project", "us-central1", anthropicauth.ConfigFromEnv())
	_ = client
}
