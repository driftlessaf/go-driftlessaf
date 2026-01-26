/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestIsValidIdentifier_Valid(t *testing.T) {
	tests := []string{
		"a",
		"Z",
		"abc",
		"ABC",
		"test123",
		"test_",
		"test_case",
		"CamelCase",
		"snake_case",
		"SCREAMING_SNAKE_CASE",
		"a1b2c3",
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			result := isValidIdentifier(input)
			if !result {
				t.Errorf("isValidIdentifier(%q): got = false, wanted = true", input)
			}
		})
	}
}

func TestIsValidIdentifier_Invalid(t *testing.T) {
	tests := []string{
		"",
		" ",
		"123test",   // Cannot start with digit
		"_test",     // Cannot start with underscore
		"_",         // Cannot start with underscore
		"__private", // Cannot start with underscore
		"test-case",
		"test.case",
		"test case",
		"test!",
		"test@host",
		"test#tag",
		"test$var",
		"test%mod",
		"test&ref",
		"test*ptr",
		"test+plus",
		"test=equals",
		"test/path",
		"test\\path",
		"test|pipe",
		"test~tilde",
		"test`tick",
		"test'quote",
		`test"quote`,
		"test,comma",
		"test;semi",
		"test:colon",
		"test<less",
		"test>greater",
		"test?question",
		"test[bracket",
		"test]bracket",
		"test{brace",
		"test}brace",
		"test(paren",
		"test)paren",
		" leadingSpace",
		"trailingSpace ",
		"{{nested",
		"nested}}",
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			result := isValidIdentifier(input)
			if result {
				t.Errorf("isValidIdentifier(%q): got = true, wanted = false", input)
			}
		})
	}
}

func TestWalkTemplate(t *testing.T) {
	// Generate pseudo-random values for testing value preservation
	r := rand.New(rand.NewSource(42)) // Deterministic seed for reproducible tests
	val1 := fmt.Sprintf("val-%d", r.Int63())
	val2 := fmt.Sprintf("val-%d", r.Int63())
	val3 := fmt.Sprintf("val-%d", r.Int63())
	val4 := fmt.Sprintf("val-%d", r.Int63())
	val5 := fmt.Sprintf("val-%d", r.Int63())

	tests := []struct {
		name     string
		template string
		resolver map[string]string // Maps binding names to their replacements
		expected string
		wantErr  bool
		errorMsg string
	}{{ // Valid templates
		name:     "no bindings",
		template: "This is a simple template",
		resolver: map[string]string{},
		expected: "This is a simple template",
	}, {
		name:     "single binding",
		template: "Hello {{name}}!",
		resolver: map[string]string{"name": val1},
		expected: "Hello " + val1 + "!",
	}, {
		name:     "multiple bindings",
		template: "{{greeting}} {{name}}, how are {{you}}?",
		resolver: map[string]string{
			"greeting": val1,
			"name":     val2,
			"you":      val3,
		},
		expected: val1 + " " + val2 + ", how are " + val3 + "?",
	}, {
		name:     "adjacent bindings",
		template: "{{a}}{{b}}{{c}}",
		resolver: map[string]string{
			"a": val1,
			"b": val2,
			"c": val3,
		},
		expected: val1 + val2 + val3,
	}, {
		name:     "binding at start",
		template: "{{start}} of template",
		resolver: map[string]string{"start": val1},
		expected: val1 + " of template",
	}, {
		name:     "binding at end",
		template: "End of {{template}}",
		resolver: map[string]string{"template": val1},
		expected: "End of " + val1,
	}, {
		name:     "repeated binding",
		template: "{{x}} and {{x}} and {{x}}",
		resolver: map[string]string{"x": val1},
		expected: val1 + " and " + val1 + " and " + val1,
	}, {
		name:     "preserve unknown binding",
		template: "Known {{known}} and unknown {{unknown}}",
		resolver: map[string]string{"known": val1},
		expected: "Known " + val1 + " and unknown {{unknown}}",
	}, {
		name:     "underscores in binding",
		template: "{{first_name}} {{last_name}}",
		resolver: map[string]string{
			"first_name": val4,
			"last_name":  val5,
		},
		expected: val4 + " " + val5,
	}, {
		name:     "numbers in binding",
		template: "{{test123}} {{test456}}",
		resolver: map[string]string{
			"test123": val1,
			"test456": val2,
		},
		expected: val1 + " " + val2,
	}, { // Malformed templates (no closing braces)
		name:     "unclosed binding at end",
		template: "This is {{unclosed",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: "unclosed binding: missing '}}'",
	}, {
		name:     "unclosed binding in middle",
		template: "Start {{ middle and end",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: "unclosed binding: missing '}}'",
	}, { // Invalid identifiers (should error)
		name:     "empty binding",
		template: "Empty {{}} binding",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: `invalid binding identifier ""`,
	}, {
		name:     "hyphen in binding",
		template: "Invalid {{test-case}}",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: `invalid binding identifier "test-case"`,
	}, {
		name:     "dot in binding",
		template: "Invalid {{test.value}}",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: `invalid binding identifier "test.value"`,
	}, {
		name:     "space in binding",
		template: "Invalid {{test case}}",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: `invalid binding identifier "test case"`,
	}, {
		name:     "special chars",
		template: "Invalid {{test!}}",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: `invalid binding identifier "test!"`,
	}, {
		name:     "nested braces",
		template: "{{{{nested}}}}",
		resolver: map[string]string{},
		wantErr:  true,
		errorMsg: `invalid binding identifier "{{nested"`,
	}, { // Edge cases
		name:     "just binding",
		template: "{{only}}",
		resolver: map[string]string{"only": val1},
		expected: val1,
	}, {
		name:     "empty template",
		template: "",
		resolver: map[string]string{},
		expected: "",
	}, {
		name:     "partial braces",
		template: "{ not a binding } but {{this}} is",
		resolver: map[string]string{"this": val2},
		expected: "{ not a binding } but " + val2 + " is",
	}, {
		name:     "resolver returns error",
		template: "Test {{error}} case",
		resolver: map[string]string{}, // Will trigger error in resolver
		wantErr:  true,
		errorMsg: "resolver error for",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a resolver function that uses the map or returns an error
			resolve := func(name string) (string, error) {
				if tc.name == "resolver returns error" && name == "error" {
					return "", fmt.Errorf("resolver error for %s", name)
				}
				if val, exists := tc.resolver[name]; exists {
					return val, nil
				}
				// Return the original placeholder for unknown bindings
				return fmt.Sprintf("{{%s}}", name), nil
			}

			result, err := walkTemplate(tc.template, resolve)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("walkTemplate() expected error but got nil")
				}
				if !strings.Contains(err.Error(), tc.errorMsg) {
					t.Errorf("walkTemplate() error = %v, wanted to contain %q", err, tc.errorMsg)
				}
			} else {
				if err != nil {
					t.Fatalf("walkTemplate() unexpected error = %v", err)
				}
				if result != tc.expected {
					t.Errorf("walkTemplate() result:\ngot  = %q\nwant = %q", result, tc.expected)
				}
			}
		})
	}
}

func TestWalkTemplateConsistency(t *testing.T) {
	// Test that calling walkTemplate twice with the same inputs produces the same results
	templates := []string{
		"Simple {{test}}",
		"Multiple {{a}} and {{b}}",
		"{{start}} middle {{end}}",
		"No bindings at all",
		"",
	}

	for _, template := range templates {
		t.Run(template, func(t *testing.T) {
			// Identity resolver - returns the same placeholder
			identity := func(name string) (string, error) {
				return fmt.Sprintf("{{%s}}", name), nil
			}

			result1, err1 := walkTemplate(template, identity)
			result2, err2 := walkTemplate(template, identity)
			if err1 != nil || err2 != nil {
				t.Fatalf("unexpected errors: err1=%v, err2=%v", err1, err2)
			}

			if result1 != result2 {
				t.Errorf("inconsistent results:\nresult1 = %q\nresult2 = %q", result1, result2)
			}

			// Result should be identical to input with identity resolver
			if result1 != template {
				t.Errorf("identity resolver changed template:\ninput  = %q\noutput = %q", template, result1)
			}
		})
	}
}
