/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package claudebackend resolves the Messages service, model ID, and provider
// metadata used to construct Claude executors.
//
// CLAUDE_BACKEND explicitly selects Vertex AI, the Anthropic first-party API,
// or AWS Bedrock Mantle. When unset, Resolve preserves the existing behavior:
// an Anthropic federation profile selects the first-party API and otherwise
// Vertex AI is used. Bedrock maps provider-neutral claude-* model IDs to
// Mantle's anthropic.claude-* IDs and accepts only authentication configurations
// accepted by the awsauth package. Keeping backend selection in this package
// ensures every Claude agent uses the same transport, model normalization, and
// telemetry provider without coupling AWS workload-identity lifecycle
// management to the Claude transport.
package claudebackend
