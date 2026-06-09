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
// federation rule ID and the organization ID: both must be set to select the
// Anthropic-direct backend; anything less keeps the Vertex default. The
// identity-token source is resolved separately (and defaults to Google).
func ExampleConfig_Configured() {
	fmt.Println(anthropicauth.Config{}.Configured())
	fmt.Println(anthropicauth.Config{
		FederationRuleID: "fdrl_0123456789",
		OrganizationID:   "12345678-1234-1234-1234-123456789012",
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

// ExampleConfig_ResolveSource demonstrates identity-token source selection: an
// explicit Source wins, the GitHub Actions endpoint is auto-detected when its
// ambient env is present, a token file comes next, and Google (the Cloud Run /
// GCP metadata-server path) is the default when nothing else is configured.
func ExampleConfig_ResolveSource() {
	fmt.Println(anthropicauth.Config{}.ResolveSource())
	fmt.Println(anthropicauth.Config{IdentityTokenFile: "/run/oidc/token"}.ResolveSource())
	fmt.Println(anthropicauth.Config{Source: anthropicauth.SourceGitHubActions}.ResolveSource())
	// Output:
	// google
	// file
	// github-actions
}

// ExampleModelID demonstrates mapping Vertex-style model identifiers to their
// first-party API form by stripping the "@version" suffix.
func ExampleModelID() {
	fmt.Println(anthropicauth.ModelID("claude-sonnet-4-6@default"))
	fmt.Println(anthropicauth.ModelID("claude-haiku-4-5"))
	// Output:
	// claude-sonnet-4-6
	// claude-haiku-4-5
}
