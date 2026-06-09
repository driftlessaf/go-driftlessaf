/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package anthropicauth builds the anthropic.Client used by the Claude
// agent and judge executors, selecting the authentication backend from a
// caller-supplied Config.
//
// By default (an empty Config) it uses Vertex AI (Application Default
// Credentials), exactly as before. When the Config carries the Anthropic-direct
// first-party API + Workload Identity Federation fields, it instead constructs
// a client that exchanges an OIDC identity token (read from a file) for a
// short-lived Anthropic access token via option.WithFederationTokenProvider.
//
// Config is passed as a parameter rather than read from the environment inside
// the constructor, so the library stays configurable and testable. Binaries and
// entrypoints that want the env-driven behavior call ConfigFromEnv to build the
// Config. Selection is opt-in (see Config.Configured): unless the federation
// rule ID and identity-token file are both set, the Vertex path is used
// unchanged. This keeps behavior identical until the Anthropic Console resources
// (issuer / service-account / federation-rule IDs, see DEV-1839) exist and the
// eval workflow populates the env vars.
package anthropicauth
