/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package claudebackend resolves the Messages service, model ID, and provider
// metadata used to construct Claude executors.
//
// With no Anthropic profile configured, Resolve selects Vertex AI and preserves
// the Vertex model ID. When an Anthropic federation profile is configured, it
// selects the Anthropic first-party API and removes the Vertex model version.
// Keeping this decision in one package ensures every Claude agent uses the same
// transport, model normalization, and telemetry provider.
package claudebackend
