/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package result_test

import (
	"fmt"
	"log"

	"chainguard.dev/driftlessaf/agents/result"
)

// ExampleExtractJSON demonstrates extracting JSON from various markdown formats.
func ExampleExtractJSON() {
	// Standard markdown code block
	response := `Here is the requested data:

` + "```json" + `
{
	"status": "success",
	"count": 42
}
` + "```" + `

Additional information follows.`

	json := result.ExtractJSON(response)
	fmt.Println(json)

	// Output:
	// {
	// 	"status": "success",
	// 	"count": 42
	// }
}

// ExampleExtractJSON_plainJSON demonstrates extraction when no markdown is present.
func ExampleExtractJSON_plainJSON() {
	// Plain JSON without markdown
	response := `{"message": "Hello, World!", "timestamp": 1234567890}`

	json := result.ExtractJSON(response)
	fmt.Println(json)

	// Output:
	// {"message": "Hello, World!", "timestamp": 1234567890}
}

// ExampleExtractJSON_emptyBlock demonstrates handling of empty JSON blocks.
func ExampleExtractJSON_emptyBlock() {
	// Empty JSON block
	response := `The operation failed.

` + "```json" + `
` + "```" + `

No data available.`

	json := result.ExtractJSON(response)
	fmt.Printf("Result: %q\n", json)

	// Output:
	// Result: ""
}

// ExampleExtract demonstrates type-safe extraction and unmarshaling.
func ExampleExtract() {
	// AI response with structured data
	response := `I've analyzed your request. Here are the results:

` + "```json" + `
{
	"analysis": {
		"sentiment": "positive",
		"confidence": 0.95,
		"keywords": ["golang", "example", "documentation"]
	},
	"metadata": {
		"model": "gpt-4",
		"timestamp": "2024-01-15T10:30:00Z"
	}
}
` + "```" + `

This analysis is based on the provided context.`

	// Define the expected structure
	type Response struct {
		Analysis struct {
			Sentiment  string   `json:"sentiment"`
			Confidence float64  `json:"confidence"`
			Keywords   []string `json:"keywords"`
		} `json:"analysis"`
		Metadata struct {
			Model     string `json:"model"`
			Timestamp string `json:"timestamp"`
		} `json:"metadata"`
	}

	// Extract and unmarshal
	data, err := result.Extract[Response](response)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Sentiment: %s (%.0f%% confidence)\n",
		data.Analysis.Sentiment,
		data.Analysis.Confidence*100)
	fmt.Printf("Keywords: %v\n", data.Analysis.Keywords)

	// Output:
	// Sentiment: positive (95% confidence)
	// Keywords: [golang example documentation]
}

// ExampleExtract_errorHandling demonstrates error handling during extraction.
func ExampleExtract_errorHandling() {
	// Response with invalid JSON
	response := `Here's the data:

` + "```json" + `
{
	"status": "error",
	"message": "Missing closing brace"
` + "```" + `
`

	type Status struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}

	_, err := result.Extract[Status](response)
	if err != nil {
		fmt.Printf("Error: JSON parsing failed\n")
	}

	// Output:
	// Error: JSON parsing failed
}

// ExampleExtract_fileOperations demonstrates extracting file modification instructions.
func ExampleExtract_fileOperations() {
	// AI response with file modifications
	response := `I'll fix the issue in your code:

` + "```json" + `
{
	"files": [
		{
			"filename": "main.go",
			"action": "modify",
			"content": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, World!\")\n}"
		},
		{
			"filename": "go.mod",
			"action": "create",
			"content": "module example\n\ngo 1.21"
		}
	],
	"summary": "Added missing import and created go.mod file"
}
` + "```" + `

These changes should resolve the compilation error.`

	type FileOperation struct {
		Files []struct {
			Filename string `json:"filename"`
			Action   string `json:"action"`
			Content  string `json:"content"`
		} `json:"files"`
		Summary string `json:"summary"`
	}

	ops, err := result.Extract[FileOperation](response)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Summary: %s\n", ops.Summary)
	for _, file := range ops.Files {
		fmt.Printf("- %s file: %s\n", file.Action, file.Filename)
	}

	// Output:
	// Summary: Added missing import and created go.mod file
	// - modify file: main.go
	// - create file: go.mod
}

// ExampleExtract_arrayResponse demonstrates extracting array responses.
func ExampleExtract_arrayResponse() {
	// AI response with array of items
	response := `Here are the search results:

` + "```json" + `
[
	{"id": 1, "title": "Introduction to Go", "score": 0.98},
	{"id": 2, "title": "Advanced Go Patterns", "score": 0.85},
	{"id": 3, "title": "Go Best Practices", "score": 0.79}
]
` + "```" + `
`

	type SearchResult struct {
		ID    int     `json:"id"`
		Title string  `json:"title"`
		Score float64 `json:"score"`
	}

	results, err := result.Extract[[]SearchResult](response)
	if err != nil {
		log.Fatal(err)
	}

	for _, r := range results {
		fmt.Printf("%d. %s (score: %.2f)\n", r.ID, r.Title, r.Score)
	}

	// Output:
	// 1. Introduction to Go (score: 0.98)
	// 2. Advanced Go Patterns (score: 0.85)
	// 3. Go Best Practices (score: 0.79)
}

// ExampleExtractJSON_multipleBlocks demonstrates behavior with multiple JSON blocks.
func ExampleExtractJSON_multipleBlocks() {
	// Response with multiple JSON blocks (only first is extracted)
	response := `First dataset:

` + "```json" + `
{"dataset": 1, "value": "first"}
` + "```" + `

Second dataset:

` + "```json" + `
{"dataset": 2, "value": "second"}
` + "```" + `
`

	// ExtractJSON returns the first JSON block found
	json := result.ExtractJSON(response)
	fmt.Println(json)

	// Output:
	// {"dataset": 1, "value": "first"}
}

// ExampleExtract_complexStructure demonstrates extracting deeply nested structures.
func ExampleExtract_complexStructure() {
	// AI response with complex nested structure
	response := `Analysis complete:

` + "```json" + `
{
	"project": {
		"name": "awesome-app",
		"language": "go",
		"dependencies": {
			"direct": [
				{"name": "github.com/gorilla/mux", "version": "v1.8.0"},
				{"name": "github.com/stretchr/testify", "version": "v1.8.4"}
			],
			"indirect": 15
		},
		"metrics": {
			"lines_of_code": 5420,
			"test_coverage": 87.3,
			"complexity": {
				"average": 3.2,
				"max": 12
			}
		}
	}
}
` + "```" + `
`

	type ProjectAnalysis struct {
		Project struct {
			Name         string `json:"name"`
			Language     string `json:"language"`
			Dependencies struct {
				Direct []struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"direct"`
				Indirect int `json:"indirect"`
			} `json:"dependencies"`
			Metrics struct {
				LinesOfCode  int     `json:"lines_of_code"`
				TestCoverage float64 `json:"test_coverage"`
				Complexity   struct {
					Average float64 `json:"average"`
					Max     int     `json:"max"`
				} `json:"complexity"`
			} `json:"metrics"`
		} `json:"project"`
	}

	analysis, err := result.Extract[ProjectAnalysis](response)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Project: %s (%s)\n", analysis.Project.Name, analysis.Project.Language)
	fmt.Printf("Test Coverage: %.1f%%\n", analysis.Project.Metrics.TestCoverage)
	fmt.Printf("Direct Dependencies: %d\n", len(analysis.Project.Dependencies.Direct))

	// Output:
	// Project: awesome-app (go)
	// Test Coverage: 87.3%
	// Direct Dependencies: 2
}
