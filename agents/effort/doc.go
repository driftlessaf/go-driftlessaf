/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package effort defines the provider-neutral reasoning-effort scale shared
// by the Claude, Gemini, and OpenAI-compatible executor backends.
//
// A single Level value expresses how deeply a model should think regardless
// of which backend runs the agent, so swapping models does not require
// retuning backend-specific knobs. Each executor exposes a WithEffort option
// that maps the level onto its provider's native control; see the Level
// documentation for the per-backend mappings.
package effort
