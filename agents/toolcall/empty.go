/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

// EmptyTools is the base tools type with no callbacks.
// Use this as the foundation when composing tool stacks.
type EmptyTools struct{}

// emptyToolsProvider provides no tools.
// Use this as the base when composing tool provider stacks.
type emptyToolsProvider[Resp any] struct{}

var _ ToolProvider[any, EmptyTools] = (*emptyToolsProvider[any])(nil)

// NewEmptyToolsProvider returns a ToolProvider that provides no tools.
// Use this as the base when composing tool provider stacks.
func NewEmptyToolsProvider[Resp any]() ToolProvider[Resp, EmptyTools] {
	return emptyToolsProvider[Resp]{}
}

func (emptyToolsProvider[Resp]) Tools(_ EmptyTools) map[string]Tool[Resp] {
	return map[string]Tool[Resp]{}
}
