/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package anthropicauth builds the anthropic.Client used by the Claude
// agent and judge executors, selecting the authentication backend from a
// caller-supplied Config.
//
// With an empty Config it uses Vertex AI (Application Default Credentials).
// When the Config carries the Anthropic-direct first-party API + Workload
// Identity Federation fields (see Config.Configured: the federation rule and
// organization IDs), it instead constructs a client that exchanges an OIDC
// identity token for a short-lived Anthropic access token via
// option.WithFederationTokenProvider.
//
// The identity token comes from a pluggable source (see IdentityTokenSource):
// the GitHub Actions runner endpoint in CI, a sidecar-refreshed file, or a
// Google-issued token from the GCP metadata server on Cloud Run. The source
// is auto-detected from the environment and can be forced via Config.Source.
//
// Anthropic's token endpoint treats each identity assertion as single-use, so
// the federation client is constructed at most once per Config and shared by
// all callers in the process: one token exchange, one cached access token.
//
// Config is passed as a parameter rather than read from the environment inside
// the constructor, so the library stays configurable and testable. Binaries and
// entrypoints that want the env-driven behavior call ConfigFromEnv to build the
// Config. Selection is opt-in: unless the federation rule and organization IDs
// are both set, the Vertex path is used. This keeps behavior unchanged for any
// deployment that does not populate the env vars (DEV-1839 rollout lever).
package anthropicauth
