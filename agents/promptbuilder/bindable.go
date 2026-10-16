/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import "fmt"

// Bindable represents a type that can bind values to a Prompt.
// Executors expect request types to implement this interface so that
// prompts can be bound to the specific data in each request.
type Bindable interface {
	// Bind takes a prompt and returns a new prompt with bound values.
	// The implementation should bind any necessary values from the receiver to the prompt.
	// This allows executors to pass request-specific data into prompt templates.
	Bind(prompt *Prompt) (*Prompt, error)
}

// Noop is a no-op implementation of Bindable that passes through the prompt unchanged
type Noop struct{}

// Bind implements Bindable by returning the prompt unchanged
func (Noop) Bind(prompt *Prompt) (*Prompt, error) {
	return prompt, nil
}

// SuffixBindable is a request that also binds values into an agent's user
// prompt suffix, so the suffix can vary per execution while the bound user
// prompt stays identical across executions and keeps its cache entry.
type SuffixBindable interface {
	// BindSuffix returns suffix with the receiver's per-execution values
	// bound.
	BindSuffix(suffix *Prompt) (*Prompt, error)
}

// BindSuffix binds request into suffix when request is SuffixBindable, and
// returns suffix unchanged otherwise. A nil suffix stays nil.
func BindSuffix(suffix *Prompt, request any) (*Prompt, error) {
	b, ok := request.(SuffixBindable)
	if suffix == nil || !ok {
		return suffix, nil
	}
	bound, err := b.BindSuffix(suffix)
	if err != nil {
		return nil, fmt.Errorf("binding the request into the user prompt suffix: %w", err)
	}
	return bound, nil
}
