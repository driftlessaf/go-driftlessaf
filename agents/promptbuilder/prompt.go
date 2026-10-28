/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// stringLiteral is a private type alias that only accepts literal strings
type stringLiteral string

// UnorderedList is a list of single-line text items rendered as Markdown
// bullets. It is a distinct type from OrderedList, so a list declared as one
// cannot be bound as the other; plain []string values convert to either.
type UnorderedList []string

// OrderedList is a list of single-line text items rendered as a numbered
// Markdown list. It is a distinct type from UnorderedList, so a list
// declared as one cannot be bound as the other; plain []string values
// convert to either.
type OrderedList []string

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

// bind returns a copy of the Prompt with b bound at name, after verifying
// that the placeholder exists and is not already bound.
func (p *Prompt) bind(name string, b binding) (*Prompt, error) {
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	newPrompt := &Prompt{
		template: p.template,
		bindings: maps.Clone(p.bindings),
	}
	newPrompt.bindings[name] = b
	return newPrompt, nil
}

// BindStringLiteral binds a literal string value to a placeholder
// The value comes from the developer, not from user input
// Returns a new Prompt with the binding applied
func (p *Prompt) BindStringLiteral(name string, value stringLiteral) (*Prompt, error) {
	return p.bind(name, &literalBinding{val: string(value)})
}

// BindXML binds structured data to a placeholder by marshaling it as XML
// The data parameter can be any type that xml.Marshal accepts
// Returns a new Prompt with the binding applied
func (p *Prompt) BindXML(name string, data any) (*Prompt, error) {
	return p.bind(name, &xmlBinding{data: data})
}

// BindJSON binds structured data to a placeholder by marshaling it as JSON
// The data parameter can be any type that json.Marshal accepts
// Returns a new Prompt with the binding applied
func (p *Prompt) BindJSON(name string, data any) (*Prompt, error) {
	return p.bind(name, &jsonBinding{data: data})
}

// BindYAML binds structured data to a placeholder by marshaling it as YAML
// The data parameter can be any type that yaml.Marshal accepts
// Returns a new Prompt with the binding applied
func (p *Prompt) BindYAML(name string, data any) (*Prompt, error) {
	return p.bind(name, &yamlBinding{data: data})
}

// BindUnorderedList binds items as an unordered Markdown list. Items may
// carry runtime data but must be single-line: an item containing a line
// break is rejected, so every item renders as exactly one bullet.
func (p *Prompt) BindUnorderedList(name string, items UnorderedList) (*Prompt, error) {
	return p.bindList(name, listBinding{items: slices.Clone(items)})
}

// BindOrderedList binds items as a numbered Markdown list. Items may carry
// runtime data but must be single-line: an item containing a line break is
// rejected, so every item renders as exactly one list entry.
func (p *Prompt) BindOrderedList(name string, items OrderedList) (*Prompt, error) {
	return p.bindList(name, listBinding{items: slices.Clone(items), ordered: true})
}

func (p *Prompt) bindList(name string, binding listBinding) (*Prompt, error) {
	// Check the placeholder before the items so error precedence matches the
	// other Bind methods.
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	for i, item := range binding.items {
		if strings.ContainsFunc(item, isLineBreak) {
			return nil, fmt.Errorf("list item %d for %q contains a line break; items must be single-line", i, name)
		}
	}
	return p.bind(name, &binding)
}

// BindPrompt binds another Prompt's Build output to a placeholder. The inner
// Prompt is built each time the outer one is, so unresolved bindings inside
// inner surface as Build errors on the outer. Use this to compose Prompts when
// the content at a placeholder is itself produced by a (literal-built) Prompt.
func (p *Prompt) BindPrompt(name string, other *Prompt) (*Prompt, error) {
	// Check the placeholder before other so error precedence matches the
	// other Bind methods.
	if err := existsAndUnbound(p.bindings, name); err != nil {
		return nil, err
	}
	if other == nil {
		return nil, fmt.Errorf("BindPrompt: prompt for %q is nil", name)
	}
	return p.bind(name, &promptBinding{p: other})
}

// maxCompositionDepth bounds how deeply BindPrompt compositions can nest, so
// a pathologically deep chain surfaces as a Build error.
const maxCompositionDepth = 100

// maxBuildBytes bounds the total rendered output of one Build traversal,
// counted per placeholder occurrence across every composed prompt, so an
// output-multiplying template or composition surfaces as an error while the
// aggregate held in memory stays bounded.
const maxBuildBytes = 64 << 20

// buildState carries the composition guards through one Build traversal:
// depth follows the current path, bytes accumulates rendered output across
// the traversal, and memo holds each composed prompt's built output so a
// prompt bound at several placeholders builds once.
type buildState struct {
	depth int
	bytes int
	memo  map[*Prompt]string
}

// Build constructs the final prompt, returning an error if any bindings are
// unbound or the rendered output exceeds the build size limit.
func (p *Prompt) Build() (string, error) {
	return p.build(&buildState{memo: map[*Prompt]string{}})
}

func (p *Prompt) build(st *buildState) (string, error) {
	if st.depth > maxCompositionDepth {
		return "", fmt.Errorf("prompt composition exceeds %d nested prompts", maxCompositionDepth)
	}

	// Pre-compute all binding values to check for errors and avoid recomputation
	values := make(map[string]string, len(p.bindings))
	for name, binding := range p.bindings {
		val, err := binding.value(st)
		if err != nil {
			return "", err
		}
		values[name] = val
	}

	// Use the same walkTemplate function for consistent tokenization. A
	// placeholder is substituted at every occurrence, so the size guard
	// accumulates per occurrence, across the whole traversal.
	st.bytes += len(p.template)
	if st.bytes > maxBuildBytes {
		return "", fmt.Errorf("prompt build exceeds %d bytes", maxBuildBytes)
	}
	return walkTemplate(p.template, func(name string) (string, error) {
		val, exists := values[name]
		if !exists {
			// This should be unreachable since NewPrompt and Build use the same walkTemplate logic
			return "", fmt.Errorf("internal error: binding %q not found in values map", name)
		}
		st.bytes += len(val)
		if st.bytes > maxBuildBytes {
			return "", fmt.Errorf("prompt build exceeds %d bytes", maxBuildBytes)
		}
		return val, nil
	})
}
