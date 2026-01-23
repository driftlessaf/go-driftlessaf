/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"encoding/json"
	"encoding/xml"
	"fmt"

	"gopkg.in/yaml.v3"
)

// binding represents a value that will be substituted into the template
type binding interface {
	value() (string, error)
}

// unboundBinding is the default state for bindings that haven't been set
type unboundBinding struct {
	name string
}

func (u *unboundBinding) value() (string, error) {
	return "", fmt.Errorf("unbound placeholder: %s", u.name)
}

// literalBinding holds a literal string value from the developer
type literalBinding struct {
	val string
}

func (l *literalBinding) value() (string, error) {
	return l.val, nil
}

// xmlBinding holds structured data to be marshaled as XML
type xmlBinding struct {
	data any
}

func (x *xmlBinding) value() (string, error) {
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

func (j *jsonBinding) value() (string, error) {
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

func (y *yamlBinding) value() (string, error) {
	bytes, err := yaml.Marshal(y.data)
	if err != nil {
		return "", fmt.Errorf("failed to marshal YAML: %w", err)
	}
	return string(bytes), nil
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
