/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

// This file contains syntactic sugar helpers that panic on error,
// useful for package-level variables and situations where templates
// are known to be valid at compile time.

// Must is a helper that wraps a call to a function returning (*Prompt, error)
// and panics if the error is non-nil. It is intended for use in variable
// initializations such as:
//
//	var p = promptbuilder.Must(promptbuilder.NewPrompt(`Hello {{name}}`))
func Must(p *Prompt, err error) *Prompt {
	if err != nil {
		panic(err)
	}
	return p
}

// MustNewPrompt creates a new prompt from a template literal and panics on error.
// This is syntactic sugar for Must(NewPrompt(...))
func MustNewPrompt(template stringLiteral) *Prompt {
	return Must(NewPrompt(template))
}

// MustBindStringLiteral binds a literal string value to a placeholder and panics on error.
// This is syntactic sugar for Must(p.BindStringLiteral(...))
func (p *Prompt) MustBindStringLiteral(name string, value stringLiteral) *Prompt {
	return Must(p.BindStringLiteral(name, value))
}

// MustBindXML binds structured data as XML to a placeholder and panics on error.
// This is syntactic sugar for Must(p.BindXML(...))
func (p *Prompt) MustBindXML(name string, data any) *Prompt {
	return Must(p.BindXML(name, data))
}

// MustBindJSON binds structured data as JSON to a placeholder and panics on error.
// This is syntactic sugar for Must(p.BindJSON(...))
func (p *Prompt) MustBindJSON(name string, data any) *Prompt {
	return Must(p.BindJSON(name, data))
}

// MustBindYAML binds structured data as YAML to a placeholder and panics on error.
// This is syntactic sugar for Must(p.BindYAML(...))
func (p *Prompt) MustBindYAML(name string, data any) *Prompt {
	return Must(p.BindYAML(name, data))
}
