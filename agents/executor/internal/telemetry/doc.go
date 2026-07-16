/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package telemetry provides the shared GenAI metrics recorder used by the
// executor implementations (claudeexecutor, googleexecutor, openaiexecutor)
// so each backend emits identically-shaped metrics, differing only in the
// gen_ai.provider.name attribute.
package telemetry
