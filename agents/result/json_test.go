/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package result

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{{
		name: "simple json block",
		input: `Here is the response:
` + "```json" + `
{"key": "value"}
` + "```",
		expected: `{"key": "value"}`,
	}, {
		name: "json block with multiple lines",
		input: "```json\n" +
			`{
  "files": [
    {
      "filename": "test.go",
      "content": "package main"
    }
  ],
  "description": "Fixed the issue"
}` + "\n```",
		expected: `{
  "files": [
    {
      "filename": "test.go",
      "content": "package main"
    }
  ],
  "description": "Fixed the issue"
}`,
	}, {
		name: "json block with text before and after",
		input: `Let me analyze this issue.

` + "```json" + `
{"error": "something went wrong"}
` + "```" + `

That's the error we need to fix.`,
		expected: `{"error": "something went wrong"}`,
	}, {
		name:     "empty json block",
		input:    "```json\n```",
		expected: "",
	}, {
		name:     "json block with only whitespace",
		input:    "```json\n   \n\t\n```",
		expected: "",
	}, {
		name:     "no json block - plain json",
		input:    `{"plain": "json"}`,
		expected: `{"plain": "json"}`,
	}, {
		name: "no json block - json with whitespace",
		input: `
    {"plain": "json", "with": "whitespace"}
    `,
		expected: `{"plain": "json", "with": "whitespace"}`,
	}, {
		name:     "malformed json block - no closing marker",
		input:    "```json\n{\"incomplete\": true",
		expected: `{"incomplete": true`,
	}, {
		name:     "multiple json blocks - returns first one",
		input:    "```json\n{\"first\": true}\n```\n\nSome text\n\n```json\n{\"second\": true}\n```",
		expected: `{"first": true}`,
	}, {
		name:     "json block with backticks in content",
		input:    "```json\n{\"content\": \"has ` backtick\"}\n```",
		expected: `{"content": "has ` + "`" + ` backtick"}`,
	}, {
		name:     "generic code block without json marker",
		input:    "```\n{\"generic\": \"block\"}\n```",
		expected: `{"generic": "block"}`,
	}, {
		name:     "inline markdown style json block",
		input:    "```json{\"inline\": \"style\"}```",
		expected: `{"inline": "style"}`,
	}, {
		name: "json block with nested quotes",
		input: `Here's the fix:
` + "```json" + `
{
  "message": "He said \"Hello, World!\"",
  "escaped": "Line 1\nLine 2"
}
` + "```",
		expected: `{
  "message": "He said \"Hello, World!\"",
  "escaped": "Line 1\nLine 2"
}`,
	}, {
		name:     "windows-style line endings",
		input:    "```json\r\n{\"windows\": \"style\"}\r\n```",
		expected: `{"windows": "style"}`,
	}, {
		name:     "json block with array",
		input:    "```json\n[1, 2, 3, 4, 5]\n```",
		expected: `[1, 2, 3, 4, 5]`,
	}, {
		name:     "json block with null and boolean",
		input:    "```json\n{\"null_value\": null, \"bool_value\": true}\n```",
		expected: `{"null_value": null, "bool_value": true}`,
	}, {
		name:     "code block marker not on own line",
		input:    "The response is ```json{\"inline\": true}``` here",
		expected: "The response is ```json{\"inline\": true}``` here",
	}, {
		name:     "json block with special characters",
		input:    "```json\n{\"special\": \"chars: <>&'\\\"\"}\n```",
		expected: `{"special": "chars: <>&'\""}`,
	}, {
		name:     "empty input",
		input:    "",
		expected: "",
	}, {
		name:     "only whitespace",
		input:    "   \n\t\n   ",
		expected: "",
	}, {
		name:     "json block with trailing comma",
		input:    "```json\n{\"key\": \"value\",}\n```",
		expected: `{"key": "value",}`,
	}, {
		name: "deeply nested json",
		input: "```json\n" + `{
  "level1": {
    "level2": {
      "level3": {
        "value": "deep"
      }
    }
  }
}` + "\n```",
		expected: `{
  "level1": {
    "level2": {
      "level3": {
        "value": "deep"
      }
    }
  }
}`,
	}, {
		name:     "json with unicode characters",
		input:    "```json\n{\"emoji\": \"🎉\", \"chinese\": \"你好\", \"arabic\": \"مرحبا\"}\n```",
		expected: `{"emoji": "🎉", "chinese": "你好", "arabic": "مرحبا"}`,
	}, {
		name:     "multiple empty lines in json block",
		input:    "```json\n\n\n{\"key\": \"value\"}\n\n\n```",
		expected: "{\"key\": \"value\"}",
	}, {
		name:     "json block with tabs",
		input:    "```json\n{\n\t\"key\":\t\"value\"\n}\n```",
		expected: "{\n\t\"key\":\t\"value\"\n}",
	}, {
		name:     "case sensitive json marker",
		input:    "```JSON\n{\"wrong\": \"case\"}\n```",
		expected: "JSON\n{\"wrong\": \"case\"}",
	}, {
		name:     "json block with comment-like content",
		input:    "```json\n{\"comment\": \"// not a real comment\"}\n```",
		expected: `{"comment": "// not a real comment"}`,
	}, {
		name:     "very large json block",
		input:    "```json\n" + strings.Repeat(`{"item": "value", "number": 123},`, 100) + "\n```",
		expected: strings.Repeat(`{"item": "value", "number": 123},`, 100),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractJSON(tt.input)
			if got != tt.expected {
				t.Errorf("ExtractJSON() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestExtractJSON_EdgeCases(t *testing.T) {
	// Test that the function handles the second code path (inline markdown style)
	t.Run("inline markdown json prefix and suffix", func(t *testing.T) {
		input := "```json{\"test\": true}```"
		expected := `{"test": true}`
		got := ExtractJSON(input)
		if got != expected {
			t.Errorf("ExtractJSON() = %q, want %q", got, expected)
		}
	})

	t.Run("inline generic code block", func(t *testing.T) {
		input := "```{\"generic\": true}```"
		expected := `{"generic": true}`
		got := ExtractJSON(input)
		if got != expected {
			t.Errorf("ExtractJSON() = %q, want %q", got, expected)
		}
	})

	// Test with mixed line endings
	t.Run("mixed line endings", func(t *testing.T) {
		input := "```json\r\n{\"line1\": true,\n\"line2\": false,\r\n\"line3\": null}\r\n```"
		expected := "{\"line1\": true,\n\"line2\": false,\r\n\"line3\": null}"
		got := ExtractJSON(input)
		if got != expected {
			t.Errorf("ExtractJSON() = %q, want %q", got, expected)
		}
	})
}

// TestExtractJSON_Fuzzing tests the function with various malformed inputs
func TestExtractJSON_Fuzzing(t *testing.T) {
	// Test various malformed inputs to ensure no panic
	malformedInputs := []string{
		"```json",
		"```",
		"json```",
		"``````",
		"```json```json```",
		"```json\n```json\n```",
		"\n\n\n```json\n\n\n",
		"```json" + strings.Repeat("\n", 1000) + "```",
		"```json\x00\x01\x02```", // with control characters
		"```json\n{broken json\n```",
	}

	for i, input := range malformedInputs {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			// Should not panic
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ExtractJSON panicked on input: %q, panic: %v", input, r)
				}
			}()
			_ = ExtractJSON(input)
		})
	}
}

// TestExtractJSON_RealWorldExamples tests with examples that might come from AI models
func TestExtractJSON_RealWorldExamples(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "Claude-style response",
			input: `I'll help you fix this issue. Based on the error, it looks like there's a type mismatch.

Here's the fix:

` + "```json" + `
{
  "files": [
    {
      "filename": "main.go",
      "content": "package main\n\nfunc main() {\n\t// Fixed code\n}"
    }
  ],
  "description": "Fixed type mismatch in main.go"
}
` + "```" + `

This should resolve the compilation error.`,
			expected: `{
  "files": [
    {
      "filename": "main.go",
      "content": "package main\n\nfunc main() {\n\t// Fixed code\n}"
    }
  ],
  "description": "Fixed type mismatch in main.go"
}`,
		},
		{
			name: "GPT-style response",
			input: `The analysis shows multiple test failures. Here's my analysis:

` + "```json" + `
{
  "summary": "Found 3 test failures related to timeout issues",
  "failures": [
    {
      "type": "test",
      "error_message": "context deadline exceeded",
      "location": {"file_path": "handler_test.go", "line": 45}
    }
  ],
  "pages_analyzed": 5
}
` + "```",
			expected: `{
  "summary": "Found 3 test failures related to timeout issues",
  "failures": [
    {
      "type": "test",
      "error_message": "context deadline exceeded",
      "location": {"file_path": "handler_test.go", "line": 45}
    }
  ],
  "pages_analyzed": 5
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractJSON(tt.input)
			if got != tt.expected {
				t.Errorf("ExtractJSON() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestExtract tests the generic Extract function
func TestExtract(t *testing.T) {
	// Test with a simple struct
	type SimpleResponse struct {
		Message string `json:"message"`
		Count   int    `json:"count"`
	}

	// Test with a complex nested struct
	type NestedResponse struct {
		Status string `json:"status"`
		Data   struct {
			Items []struct {
				ID    int     `json:"id"`
				Name  string  `json:"name"`
				Value float64 `json:"value"`
			} `json:"items"`
			Metadata map[string]any `json:"metadata"`
		} `json:"data"`
		Errors []string `json:"errors,omitempty"`
	}

	tests := []struct {
		name        string
		input       string
		wantSimple  *SimpleResponse
		wantNested  *NestedResponse
		wantErr     bool
		errContains string
	}{
		{
			name: "simple struct extraction",
			input: `Here's the response:
` + "```json" + `
{
  "message": "Hello, World!",
  "count": 42
}
` + "```",
			wantSimple: &SimpleResponse{
				Message: "Hello, World!",
				Count:   42,
			},
		},
		{
			name: "nested struct extraction",
			input: "```json\n" + `{
  "status": "success",
  "data": {
    "items": [
      {"id": 1, "name": "Item 1", "value": 10.5},
      {"id": 2, "name": "Item 2", "value": 20.0}
    ],
    "metadata": {
      "total": 2,
      "version": "1.0"
    }
  },
  "errors": []
}` + "\n```",
			wantNested: &NestedResponse{
				Status: "success",
				Data: struct {
					Items []struct {
						ID    int     `json:"id"`
						Name  string  `json:"name"`
						Value float64 `json:"value"`
					} `json:"items"`
					Metadata map[string]any `json:"metadata"`
				}{
					Items: []struct {
						ID    int     `json:"id"`
						Name  string  `json:"name"`
						Value float64 `json:"value"`
					}{{ID: 1, Name: "Item 1", Value: 10.5}, {ID: 2, Name: "Item 2", Value: 20.0}},
					Metadata: map[string]any{
						"total":   float64(2),
						"version": "1.0",
					},
				},
				Errors: []string{},
			},
		},
		{
			name:        "empty json block error",
			input:       "```json\n```",
			wantErr:     true,
			errContains: "unexpected end of JSON input",
		},
		{
			name:        "invalid json error",
			input:       "```json\n{invalid json}\n```",
			wantErr:     true,
			errContains: "invalid character",
		},
		{
			name:        "type mismatch error",
			input:       `{"message": 123, "count": "not a number"}`,
			wantSimple:  nil,
			wantErr:     true,
			errContains: "cannot unmarshal",
		},
		{
			name:        "no json content",
			input:       "This is just plain text with no JSON",
			wantErr:     true,
			errContains: "invalid character",
		},
		{
			name:  "array extraction",
			input: "```json\n[1, 2, 3, 4, 5]\n```",
			// This will be tested separately with array type
		},
		{
			name:  "null value handling",
			input: "```json\nnull\n```",
			// This will be tested separately
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantSimple != nil {
				got, err := Extract[SimpleResponse](tt.input)
				if (err != nil) != tt.wantErr {
					t.Errorf("Extract() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				if tt.wantErr && tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Extract() error = %v, want error containing %q", err, tt.errContains)
					return
				}
				if !tt.wantErr && !reflect.DeepEqual(got, *tt.wantSimple) {
					t.Errorf("Extract() = %+v, want %+v", got, *tt.wantSimple)
				}
			}

			if tt.wantNested != nil {
				got, err := Extract[NestedResponse](tt.input)
				if (err != nil) != tt.wantErr {
					t.Errorf("Extract() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				if !tt.wantErr && !reflect.DeepEqual(got, *tt.wantNested) {
					t.Errorf("Extract() = %+v, want %+v", got, *tt.wantNested)
				}
			}

			// Generic error cases
			if tt.wantSimple == nil && tt.wantNested == nil && tt.wantErr {
				_, err := Extract[SimpleResponse](tt.input)
				if (err != nil) != tt.wantErr {
					t.Errorf("Extract() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Extract() error = %v, want error containing %q", err, tt.errContains)
				}
			}
		})
	}

	// Test with array type
	t.Run("array extraction", func(t *testing.T) {
		input := "```json\n[1, 2, 3, 4, 5]\n```"
		got, err := Extract[[]int](input)
		if err != nil {
			t.Errorf("Extract() unexpected error = %v", err)
			return
		}
		want := []int{1, 2, 3, 4, 5}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Extract() = %v, want %v", got, want)
		}
	})

	// Test with map type
	t.Run("map extraction", func(t *testing.T) {
		input := `{"key1": "value1", "key2": "value2"}`
		got, err := Extract[map[string]string](input)
		if err != nil {
			t.Errorf("Extract() unexpected error = %v", err)
			return
		}
		want := map[string]string{"key1": "value1", "key2": "value2"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Extract() = %v, want %v", got, want)
		}
	})

	// Test with pointer types
	t.Run("pointer type extraction", func(t *testing.T) {
		input := `{"message": "test", "count": 10}`
		got, err := Extract[*SimpleResponse](input)
		if err != nil {
			t.Errorf("Extract() unexpected error = %v", err)
			return
		}
		if got == nil {
			t.Error("Extract() returned nil pointer")
			return
		}
		if want := (&SimpleResponse{Message: "test", Count: 10}); !reflect.DeepEqual(got, want) {
			t.Errorf("Extract() = %+v, want %+v", got, want)
		}
	})

	// Test with interface{} type
	t.Run("interface extraction", func(t *testing.T) {
		input := `{"dynamic": true, "number": 123, "nested": {"key": "value"}}`
		got, err := Extract[any](input)
		if err != nil {
			t.Errorf("Extract() unexpected error = %v", err)
			return
		}
		// Check that it's a map
		if _, ok := got.(map[string]any); !ok {
			t.Errorf("Extract() = %T, want map[string]interface{}", got)
		}
	})
}
