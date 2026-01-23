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

func TestNoopBinding(t *testing.T) {
	t.Run("Noop returns unchanged prompt", func(t *testing.T) {
		// Create a prompt with some bindings
		prompt, err := promptbuilder.NewPrompt(`Hello {{name}}, welcome to {{place}}!`)
		if err != nil {
			t.Fatalf("NewPrompt() error = %v", err)
		}

		// Create a Noop bindable
		noop := promptbuilder.Noop{}

		// Apply Noop binding
		result, err := noop.Bind(prompt)
		if err != nil {
			t.Fatalf("Noop.Bind() error = %v", err)
		}

		// Result should be the same prompt instance
		if result != prompt {
			t.Error("Noop.Bind() should return the same prompt instance")
		}

		// Verify prompt still has same bindings
		bindings := result.GetBindings()
		if _, exists := bindings["name"]; !exists {
			t.Error("Binding 'name' should still exist after Noop")
		}
		if _, exists := bindings["place"]; !exists {
			t.Error("Binding 'place' should still exist after Noop")
		}
	})

	t.Run("Noop works with already bound prompts", func(t *testing.T) {
		// Create and bind a prompt
		prompt, err := promptbuilder.NewPrompt(`Hello {{name}}!`)
		if err != nil {
			t.Fatalf("NewPrompt() error = %v", err)
		}

		prompt, err = prompt.BindStringLiteral("name", "World")
		if err != nil {
			t.Fatalf("BindStringLiteral() error = %v", err)
		}

		// Apply Noop binding
		noop := promptbuilder.Noop{}
		result, err := noop.Bind(prompt)
		if err != nil {
			t.Fatalf("Noop.Bind() error = %v", err)
		}

		// Result should be the same prompt instance
		if result != prompt {
			t.Error("Noop.Bind() should return the same prompt instance")
		}

		// Should still build successfully
		built, err := result.Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if built != "Hello World!" {
			t.Errorf("Build() = %v, want %v", built, "Hello World!")
		}
	})

	t.Run("Noop works with empty prompt", func(t *testing.T) {
		// Create an empty prompt
		prompt, err := promptbuilder.NewPrompt(`No bindings here`)
		if err != nil {
			t.Fatalf("NewPrompt() error = %v", err)
		}

		// Apply Noop binding
		noop := promptbuilder.Noop{}
		result, err := noop.Bind(prompt)
		if err != nil {
			t.Fatalf("Noop.Bind() error = %v", err)
		}

		// Result should be the same prompt instance
		if result != prompt {
			t.Error("Noop.Bind() should return the same prompt instance")
		}

		// Should build successfully
		built, err := result.Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if built != "No bindings here" {
			t.Errorf("Build() = %v, want %v", built, "No bindings here")
		}
	})

	t.Run("Noop can be chained multiple times", func(t *testing.T) {
		// Create a prompt
		prompt, err := promptbuilder.NewPrompt(`Hello {{name}}!`)
		if err != nil {
			t.Fatalf("NewPrompt() error = %v", err)
		}

		// Apply Noop multiple times
		noop := promptbuilder.Noop{}
		result := prompt
		for i := range 3 {
			result, err = noop.Bind(result)
			if err != nil {
				t.Fatalf("Noop.Bind() iteration %d error = %v", i, err)
			}
		}

		// Result should still be the same prompt instance
		if result != prompt {
			t.Error("Multiple Noop.Bind() calls should return the same prompt instance")
		}
	})

	t.Run("Noop as embedded field", func(t *testing.T) {
		// Test that types can embed Noop to get a default Bind implementation
		type MyBindable struct {
			promptbuilder.Noop
			Value string
		}

		prompt, err := promptbuilder.NewPrompt(`Data: {{data}}`)
		if err != nil {
			t.Fatalf("NewPrompt() error = %v", err)
		}

		myBindable := MyBindable{Value: "test"}
		result, err := myBindable.Bind(prompt)
		if err != nil {
			t.Fatalf("MyBindable.Bind() error = %v", err)
		}

		// Should return unchanged prompt
		if result != prompt {
			t.Error("Embedded Noop.Bind() should return the same prompt instance")
		}

		// Bindings should remain unbound
		_, err = result.Build()
		if err == nil {
			t.Error("Build() should fail with unbound placeholder")
		}
		if !strings.Contains(err.Error(), "unbound placeholder") {
			t.Errorf("Build() error = %v, want error about unbound placeholder", err)
		}
	})

	t.Run("Noop implements Bindable interface", func(t *testing.T) {
		// Compile-time check that Noop implements Bindable
		var _ promptbuilder.Bindable = promptbuilder.Noop{}

		// Also test through interface
		var bindable promptbuilder.Bindable = promptbuilder.Noop{}

		prompt, err := promptbuilder.NewPrompt(`Test {{value}}`)
		if err != nil {
			t.Fatalf("NewPrompt() error = %v", err)
		}

		result, err := bindable.Bind(prompt)
		if err != nil {
			t.Fatalf("Bindable.Bind() error = %v", err)
		}

		if result != prompt {
			t.Error("Bindable.Bind() via interface should return the same prompt instance")
		}
	})
}
