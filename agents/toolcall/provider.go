/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
)

// ToolProvider defines tools for an agent.
// Implementations return provider-specific tool definitions.
// Compose providers by wrapping: Empty -> Worktree -> Finding.
type ToolProvider[Resp, CB any] interface {
	// ClaudeTools returns tool definitions for Claude models.
	ClaudeTools(cb CB) map[string]claudetool.Metadata[Resp]

	// GoogleTools returns tool definitions for Gemini models.
	GoogleTools(cb CB) map[string]googletool.Metadata[Resp]
}
