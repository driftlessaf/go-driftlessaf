/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// binding represents a value that will be substituted into the template.
// Only promptBinding recurses with the build state.
type binding interface {
	value(st *buildState) (string, error)
}

// unboundBinding is the default state for bindings that haven't been set
type unboundBinding struct {
	name string
}

func (u *unboundBinding) value(*buildState) (string, error) {
	return "", fmt.Errorf("unbound placeholder: %s", u.name)
}

// literalBinding holds a literal string value from the developer
type literalBinding struct {
	val string
}

func (l *literalBinding) value(*buildState) (string, error) {
	return l.val, nil
}

// xmlBinding holds structured data to be marshaled as XML
type xmlBinding struct {
	data any
}

func (x *xmlBinding) value(*buildState) (string, error) {
	bytes, err := xml.MarshalIndent(x.data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal XML: %w", err)
	}
	return string(bytes), nil
}

// jsonBinding holds structured data to be marshaled as JSON
type jsonBinding struct {
	data any
}

func (j *jsonBinding) value(*buildState) (string, error) {
	bytes, err := json.MarshalIndent(j.data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal JSON: %w", err)
	}
	return string(bytes), nil
}

// yamlBinding holds structured data to be marshaled as YAML
type yamlBinding struct {
	data any
}

func (y *yamlBinding) value(*buildState) (string, error) {
	bytes, err := yaml.Marshal(y.data)
	if err != nil {
		return "", fmt.Errorf("failed to marshal YAML: %w", err)
	}
	return string(bytes), nil
}

type listBinding struct {
	items   []string
	ordered bool
}

func (b *listBinding) value(*buildState) (string, error) {
	lines := make([]string, 0, len(b.items))
	for i, item := range b.items {
		if b.ordered {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, item))
			continue
		}
		lines = append(lines, "- "+item)
	}
	return strings.Join(lines, "\n"), nil
}

// isLineBreak reports whether r starts a new line in common renderers: the
// ASCII line controls plus the Unicode line and paragraph separators.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\u0085':
		return true
	}
	return unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// promptBinding defers to another Prompt's Build output, so a literal-built
// Prompt can be plugged in at a placeholder without going through string
// serialization.
type promptBinding struct {
	p *Prompt
}

func (b *promptBinding) value(st *buildState) (string, error) {
	if built, ok := st.memo[b.p]; ok {
		return built, nil
	}

	st.depth++
	defer func() { st.depth-- }()

	built, err := b.p.build(st)
	if err != nil {
		return "", err
	}
	st.memo[b.p] = built
	return built, nil
}

// existsAndUnbound checks if a binding exists and is currently unbound
// Returns an error if the binding doesn't exist or has already been bound
func existsAndUnbound(bindings map[string]binding, name string) error {
	b, exists := bindings[name]
	if !exists {
		return fmt.Errorf("binding %q not found in template", name)
	}
	if _, isUnbound := b.(*unboundBinding); !isUnbound {
		return fmt.Errorf("binding %q already bound", name)
	}
	return nil
}
