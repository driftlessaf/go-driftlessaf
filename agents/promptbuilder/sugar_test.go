/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder_test

import (
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

func TestMust(t *testing.T) {
	t.Run("valid template", func(t *testing.T) {
		// Should not panic
		p := promptbuilder.Must(promptbuilder.NewPrompt(`Hello {{name}}`))
		if p == nil {
			t.Error("Must returned nil for valid template")
		}
	})

	t.Run("invalid template panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("Must did not panic for invalid template")
			} else {
				// Verify we got an error about invalid identifier
				if err, ok := r.(error); ok {
					if !strings.Contains(err.Error(), "invalid binding identifier") {
						t.Errorf("unexpected panic error: %v", err)
					}
				} else {
					t.Errorf("panic value was not an error: %v", r)
				}
			}
		}()
		// This should panic due to empty binding
		promptbuilder.Must(promptbuilder.NewPrompt(`Invalid {{}}`))
	})

	t.Run("can chain with methods", func(t *testing.T) {
		// Demonstrating intended usage pattern
		p := promptbuilder.Must(promptbuilder.NewPrompt(`Hello {{name}}`))

		// Get bindings to verify template was parsed
		bindings := p.GetBindings()
		if _, exists := bindings["name"]; !exists {
			t.Error("binding 'name' not found after Must")
		}
	})
}

func TestMustNewPrompt(t *testing.T) {
	t.Run("valid template", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("MustNewPrompt() panicked with valid template: %v", r)
			}
		}()

		p := promptbuilder.MustNewPrompt(`Hello {{name}}`)
		if p == nil {
			t.Error("MustNewPrompt() returned nil for valid template")
		}
	})

	t.Run("invalid template panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("MustNewPrompt() should panic with invalid template")
			}
		}()

		// Force an error by using invalid identifier
		promptbuilder.MustNewPrompt(`{{test-invalid}}`)
	})
}

func TestMustBindStringLiteral(t *testing.T) {
	t.Run("valid binding", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("MustBindStringLiteral() panicked with valid binding: %v", r)
			}
		}()

		p := promptbuilder.MustNewPrompt(`Hello {{name}}`)
		p2 := p.MustBindStringLiteral("name", "World")
		if p2 == nil {
			t.Error("MustBindStringLiteral() returned nil for valid binding")
		}

		// New prompt should build successfully
		result, err := p2.Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if result != "Hello World" {
			t.Errorf("Build() = %v, want %v", result, "Hello World")
		}
	})

	t.Run("invalid binding panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("MustBindStringLiteral() should panic with invalid binding")
			}
		}()

		p := promptbuilder.MustNewPrompt(`Hello {{name}}`)
		// Should panic - binding doesn't exist
		p.MustBindStringLiteral("nonexistent", "value")
	})

	t.Run("chaining syntax", func(t *testing.T) {
		p := promptbuilder.MustNewPrompt(`{{greeting}} {{name}}!`)

		// Chain Must methods
		p = p.MustBindStringLiteral("greeting", "Hello")
		p = p.MustBindStringLiteral("name", "World")

		result, err := p.Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if result != "Hello World!" {
			t.Errorf("Build() = %v, want %v", result, "Hello World!")
		}
	})
}

func TestMustBindJSON(t *testing.T) {
	t.Run("valid binding", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("MustBindJSON() panicked with valid binding: %v", r)
			}
		}()

		p := promptbuilder.MustNewPrompt(`Data: {{data}}`)

		data := struct {
			Value string `json:"value"`
		}{Value: "test"}

		p2 := p.MustBindJSON("data", data)
		if p2 == nil {
			t.Error("MustBindJSON() returned nil for valid binding")
		}

		// New prompt should build successfully
		result, err := p2.Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		expected := `Data: {
  "value": "test"
}`
		if result != expected {
			t.Errorf("Build() = %v, want %v", result, expected)
		}
	})

	t.Run("invalid binding panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("MustBindJSON() should panic with invalid binding")
			}
		}()

		p := promptbuilder.MustNewPrompt(`Hello {{name}}`)
		data := struct{ Field string }{Field: "value"}
		// Should panic - binding doesn't exist
		p.MustBindJSON("nonexistent", data)
	})
}

func TestMustBindYAML(t *testing.T) {
	t.Run("valid binding", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("MustBindYAML() panicked with valid binding: %v", r)
			}
		}()

		p := promptbuilder.MustNewPrompt(`Config: {{config}}`)

		data := struct {
			Setting string `yaml:"setting"`
		}{Setting: "enabled"}

		p2 := p.MustBindYAML("config", data)
		if p2 == nil {
			t.Error("MustBindYAML() returned nil for valid binding")
		}

		// New prompt should build successfully
		result, err := p2.Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		expected := `Config: setting: enabled
`
		if result != expected {
			t.Errorf("Build() = %v, want %v", result, expected)
		}
	})

	t.Run("invalid binding panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("MustBindYAML() should panic with invalid binding")
			}
		}()

		p := promptbuilder.MustNewPrompt(`Hello {{name}}`)
		data := struct{ Field string }{Field: "value"}
		// Should panic - binding doesn't exist
		p.MustBindYAML("nonexistent", data)
	})
}

func TestMustBindXML(t *testing.T) {
	t.Run("valid binding", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("MustBindXML() panicked with valid binding: %v", r)
			}
		}()

		p := promptbuilder.MustNewPrompt(`Data: {{data}}`)

		data := struct {
			XMLName struct{} `xml:"Item"`
			Value   string   `xml:"value"`
		}{Value: "test"}

		p2 := p.MustBindXML("data", data)
		if p2 == nil {
			t.Error("MustBindXML() returned nil for valid binding")
		}

		// New prompt should build successfully
		result, err := p2.Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		expected := `Data: <Item>
  <value>test</value>
</Item>`
		if result != expected {
			t.Errorf("Build() = %v, want %v", result, expected)
		}
	})

	t.Run("invalid binding panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("MustBindXML() should panic with invalid binding")
			}
		}()

		p := promptbuilder.MustNewPrompt(`Hello {{name}}`)
		data := struct{ Field string }{Field: "value"}
		// Should panic - binding doesn't exist
		p.MustBindXML("nonexistent", data)
	})
}
