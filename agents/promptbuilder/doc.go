/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package promptbuilder provides a safe, injection-resistant prompt construction
library that leverages Go's standard encoding packages to automatically handle
escaping and formatting. Similar to SQL prepared statements, but for LLM prompts.

# Overview

The promptbuilder package allows developers to construct prompts with dynamic
content while preventing prompt injection attacks. It achieves this by:

  - Using compile-time type safety to ensure templates come from developers
  - Automatically escaping user-provided data through standard encoders
  - Preventing transitive substitutions through single-pass tokenization
  - Immutable prompt instances - all binding methods return new instances

# Basic Usage

Create a prompt template with placeholders and bind values to them:

	import "chainguard.dev/driftlessaf/agents/promptbuilder"

	// Templates must be literal strings (compile-time safety)
	p, err := promptbuilder.NewPrompt(`
		Analyze the following data:
		{{data}}

		Instructions: {{instructions}}
	`)
	if err != nil {
		// Handle invalid template error
	}

	// Bind user data as JSON (automatically escaped)
	p, err = p.BindJSON("data", userData)
	if err != nil {
		// Handle binding error
	}

	// Bind developer-controlled literal strings
	p, err = p.BindStringLiteral("instructions", "Find patterns")
	if err != nil {
		// Handle binding error
	}

	// Build the final prompt
	result, err := p.Build()
	if err != nil {
		// Handle unbound placeholder error
	}

# Binding Methods

The package provides multiple binding methods for different data formats:

	// BindStringLiteral - For developer-controlled strings only
	p, err = p.BindStringLiteral("key", "literal value")

	// BindJSON - Marshals data as indented JSON
	p, err = p.BindJSON("data", struct{Name string}{"Alice"})

	// BindXML - Marshals data as indented XML
	p, err = p.BindXML("config", xmlStruct)

	// BindYAML - Marshals data as YAML
	p, err = p.BindYAML("settings", yamlData)

Each method also has a Must variant that panics on error:

	p = p.MustBindStringLiteral("key", "value")
	p = p.MustBindJSON("data", jsonData)
	p = p.MustBindXML("config", xmlData)
	p = p.MustBindYAML("settings", yamlData)

# Template Syntax

Templates use {{name}} placeholders for bindings. Valid binding names must
contain only letters, digits, and underscores. Invalid identifiers will cause
NewPrompt to return an error.

Valid examples:
  - {{data}}
  - {{user_input}}
  - {{item1}}

Invalid examples:
  - {{}} (empty)
  - {{test-case}} (contains hyphen)
  - {{test.value}} (contains dot)

# Bindable Interface

Types can implement the Bindable interface to provide custom binding logic.
Executors expect request types to implement this interface so that prompts
can be bound to the specific data in each request:

	type Bindable interface {
		Bind(prompt *Prompt) (*Prompt, error)
	}

The package provides a Noop implementation that returns the prompt unchanged,
useful as an embedded field for types that conditionally bind values or when
no binding is needed:

	type MyRequest struct {
		promptbuilder.Noop  // Provides default Bind implementation
		Data string
	}

# Security Properties

1. No Raw User Input - User data must go through encoders (XML, JSON, or YAML)
2. Type-Safe Literals - stringLiteral ensures only developer literals bypass encoding
3. Automatic Escaping - Encoding libraries handle all escaping for user data
4. No Transitive Substitution - Single-pass tokenization prevents recursive replacement
5. Immutable Prompts - All operations return new instances, preventing mutation

# Must Functions

For convenience in package-level variables and when templates are known to be valid:

	var template = promptbuilder.MustNewPrompt(`Hello {{name}}!`)

The Must helper can wrap any (*Prompt, error) returning function:

	p := promptbuilder.Must(promptbuilder.NewPrompt(literalTemplate))

# Error Handling

The package returns errors for:
  - Invalid template syntax (malformed placeholders)
  - Binding to non-existent placeholders
  - Attempting to rebind already-bound placeholders
  - Building with unbound placeholders
  - Marshaling failures in BindJSON/BindXML/BindYAML

# Thread Safety

Prompt instances are immutable after creation. Binding methods return new instances,
making the original safe to share across goroutines. However, concurrent calls to
Build() on the same instance are safe only because the instance is immutable.
*/
package promptbuilder
