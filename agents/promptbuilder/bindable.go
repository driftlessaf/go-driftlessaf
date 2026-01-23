/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

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
