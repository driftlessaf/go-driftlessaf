/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"fmt"
	"maps"
)

// stringLiteral is a private type alias that only accepts literal strings
type stringLiteral string

// Prompt represents a template with bindable placeholders
type Prompt struct {
	template string
	bindings map[string]binding
}

// NewPrompt creates a new prompt from a template literal and parses bindings
func NewPrompt(template stringLiteral) (*Prompt, error) {
	bindings := make(map[string]binding)

	// Walk through the template and collect all bindings
	// The result should be identical to the input since we return the same placeholders
	tmpl, err := walkTemplate(string(template), func(name string) (string, error) {
		if _, exists := bindings[name]; !exists {
			bindings[name] = &unboundBinding{name: name}
		}
		// Return the original placeholder during parsing
		return fmt.Sprintf("{{%s}}", name), nil
	})
	if err != nil {
		return nil, err
	}

	return &Prompt{
		template: tmpl,
		bindings: bindings,
	}, nil
}

// GetBindings returns the names of all bindings found in the template as a set
// This is useful for testing and debugging
func (p *Prompt) GetBindings() map[string]struct{} {
	names := make(map[string]struct{}, len(p.bindings))
	for name := range p.bindings {
		names[name] = struct{}{}
	}
	return names
}

// BindStringLiteral binds a literal string value to a placeholder
// The value comes from the developer, not from user input
// Returns a new Prompt with the binding applied
func (p *Prompt) BindStringLiteral(name string, value stringLiteral) (*Prompt, error) {
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	newPrompt := &Prompt{
		template: p.template,
		bindings: maps.Clone(p.bindings),
	}
	newPrompt.bindings[name] = &literalBinding{val: string(value)}
	return newPrompt, nil
}

// BindXML binds structured data to a placeholder by marshaling it as XML
// The data parameter can be any type that xml.Marshal accepts
// Returns a new Prompt with the binding applied
func (p *Prompt) BindXML(name string, data any) (*Prompt, error) {
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	newPrompt := &Prompt{
		template: p.template,
		bindings: maps.Clone(p.bindings),
	}
	newPrompt.bindings[name] = &xmlBinding{data: data}
	return newPrompt, nil
}

// BindJSON binds structured data to a placeholder by marshaling it as JSON
// The data parameter can be any type that json.Marshal accepts
// Returns a new Prompt with the binding applied
func (p *Prompt) BindJSON(name string, data any) (*Prompt, error) {
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	newPrompt := &Prompt{
		template: p.template,
		bindings: maps.Clone(p.bindings),
	}
	newPrompt.bindings[name] = &jsonBinding{data: data}
	return newPrompt, nil
}

// BindYAML binds structured data to a placeholder by marshaling it as YAML
// The data parameter can be any type that yaml.Marshal accepts
// Returns a new Prompt with the binding applied
func (p *Prompt) BindYAML(name string, data any) (*Prompt, error) {
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	newPrompt := &Prompt{
		template: p.template,
		bindings: maps.Clone(p.bindings),
	}
	newPrompt.bindings[name] = &yamlBinding{data: data}
	return newPrompt, nil
}

// Build constructs the final prompt, returning an error if any bindings are unbound
func (p *Prompt) Build() (string, error) {
	// Pre-compute all binding values to check for errors and avoid recomputation
	values := make(map[string]string, len(p.bindings))
	for name, binding := range p.bindings {
		val, err := binding.value()
		if err != nil {
			return "", err
		}
		values[name] = val
	}

	// Use the same walkTemplate function for consistent tokenization
	return walkTemplate(p.template, func(name string) (string, error) {
		if val, exists := values[name]; exists {
			return val, nil
		}
		// This should be unreachable since NewPrompt and Build use the same walkTemplate logic
		return "", fmt.Errorf("internal error: binding %q not found in values map", name)
	})
}
