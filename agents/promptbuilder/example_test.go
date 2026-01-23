/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder_test

import (
	"fmt"
	"log"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// ExampleNewPrompt demonstrates creating a new prompt template
func ExampleNewPrompt() {
	p, err := promptbuilder.NewPrompt(`Hello {{name}}, welcome to {{service}}!`)
	if err != nil {
		log.Fatal(err)
	}

	bindings := p.GetBindings()
	fmt.Printf("Found %d bindings\n", len(bindings))
	// Output: Found 2 bindings
}

// ExampleMustNewPrompt demonstrates creating a prompt that panics on error
func ExampleMustNewPrompt() {
	// This is safe for package-level variables with known-good templates
	var template = promptbuilder.MustNewPrompt(`Analyze: {{data}}`)

	bindings := template.GetBindings()
	fmt.Printf("Template has %d binding\n", len(bindings))
	// Output: Template has 1 binding
}

// ExamplePrompt_BindStringLiteral demonstrates binding literal string values
func ExamplePrompt_BindStringLiteral() {
	p := promptbuilder.MustNewPrompt(`System: {{instructions}}
User: {{query}}`)

	// Bind developer-provided literal strings
	p, err := p.BindStringLiteral("instructions", "You are a helpful assistant.")
	if err != nil {
		log.Fatal(err)
	}

	p, err = p.BindStringLiteral("query", "What is the weather?")
	if err != nil {
		log.Fatal(err)
	}

	result, err := p.Build()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result)
	// Output: System: You are a helpful assistant.
	// User: What is the weather?
}

// ExamplePrompt_BindJSON demonstrates binding structured data as JSON
func ExamplePrompt_BindJSON() {
	p := promptbuilder.MustNewPrompt(`Process this user data:
{{user_data}}`)

	userData := map[string]any{
		"name": "Alice",
		"age":  30,
		"tags": []string{"developer", "go"},
	}

	p, err := p.BindJSON("user_data", userData)
	if err != nil {
		log.Fatal(err)
	}

	result, err := p.Build()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result)
	// Output: Process this user data:
	// {
	//   "age": 30,
	//   "name": "Alice",
	//   "tags": [
	//     "developer",
	//     "go"
	//   ]
	// }
}

// ExamplePrompt_BindXML demonstrates binding structured data as XML
func ExamplePrompt_BindXML() {
	type User struct {
		Name string `xml:"name"`
		Age  int    `xml:"age"`
	}

	p := promptbuilder.MustNewPrompt(`User profile:
{{profile}}`)

	user := User{Name: "Bob", Age: 25}

	p, err := p.BindXML("profile", user)
	if err != nil {
		log.Fatal(err)
	}

	result, err := p.Build()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result)
	// Output: User profile:
	// <User>
	//   <name>Bob</name>
	//   <age>25</age>
	// </User>
}

// ExamplePrompt_BindYAML demonstrates binding structured data as YAML
func ExamplePrompt_BindYAML() {
	p := promptbuilder.MustNewPrompt(`Configuration:
{{config}}`)

	config := map[string]any{
		"database": map[string]string{
			"host": "localhost",
			"port": "5432",
		},
		"debug": true,
	}

	p, err := p.BindYAML("config", config)
	if err != nil {
		log.Fatal(err)
	}

	result, err := p.Build()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result)
	// Output: Configuration:
	// database:
	//     host: localhost
	//     port: "5432"
	// debug: true
}

// ExamplePrompt_MustBindStringLiteral demonstrates the Must variant for binding literals
func ExamplePrompt_MustBindStringLiteral() {
	p := promptbuilder.MustNewPrompt(`Hello {{name}}!`)

	// Chain Must methods for fluent API when you know bindings will succeed
	p = p.MustBindStringLiteral("name", "World")

	result, err := p.Build()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(result)
	// Output: Hello World!
}
