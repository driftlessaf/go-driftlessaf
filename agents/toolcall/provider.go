/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

// ToolProvider defines tools for an agent.
// Implementations return provider-independent tool definitions.
// Compose providers by wrapping: Empty -> Worktree -> Finding.
// Conversion to SDK-specific types happens downstream in the metaagent layer.
type ToolProvider[Resp, CB any] interface {
	// Tools returns unified tool definitions that work with any provider.
	Tools(cb CB) map[string]Tool[Resp]
}
