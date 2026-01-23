/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package result provides utilities for extracting and parsing JSON responses from AI models.

This package simplifies the process of handling AI model responses that often contain JSON
data wrapped in markdown code blocks or other formatting. It provides type-safe extraction
and unmarshaling of JSON content from text responses.

# Overview

The result package offers the following key features:

  - Automatic extraction of JSON from markdown code blocks
  - Support for multiple markdown formats (```json, ```, inline)
  - Type-safe generic unmarshaling
  - Graceful handling of malformed responses
  - Thread-safe operations

# Basic Usage

When working with AI model responses, you often receive JSON wrapped in markdown:

	response := `Here is the data you requested:

	```json
	{
		"name": "example",
		"value": 42,
		"active": true
	}
	```
	`

	// Extract just the JSON content
	jsonStr := result.ExtractJSON(response)
	fmt.Println(jsonStr) // {"name": "example", "value": 42, "active": true}

	// Or extract and unmarshal in one step
	type Data struct {
		Name   string `json:"name"`
		Value  int    `json:"value"`
		Active bool   `json:"active"`
	}

	data, err := result.Extract[Data](response)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%+v\n", data) // {Name:example Value:42 Active:true}

# JSON Extraction

The ExtractJSON function handles various response formats:

1. Standard markdown code blocks:

	```json
	{"key": "value"}
	```

2. Generic code blocks:

	```
	{"key": "value"}
	```

3. Inline JSON (no markdown):

	{"key": "value"}

4. JSON with surrounding text:

	Here's your response:
	```json
	{"status": "success"}
	```
	Additional notes here.

# Type-Safe Extraction

The generic Extract function combines JSON extraction with unmarshaling:

	// Define your response structure
	type FixResponse struct {
		Files []struct {
			Filename string `json:"filename"`
			Content  string `json:"content"`
		} `json:"files"`
		Description string `json:"description"`
	}

	// Extract from AI response
	response := `I'll fix those files for you:

	```json
	{
		"files": [
			{
				"filename": "main.go",
				"content": "package main\n\nfunc main() {\n\t// Fixed\n}"
			}
		],
		"description": "Fixed the syntax error"
	}
	```
	`

	fix, err := result.Extract[FixResponse](response)
	if err != nil {
		return err
	}

	for _, file := range fix.Files {
		fmt.Printf("Update %s\n", file.Filename)
	}

# Error Handling

The package handles various error conditions gracefully:

	// Empty JSON block
	response := "```json\n```"
	data := result.ExtractJSON(response) // Returns ""

	// Malformed JSON
	response = "```json\n{invalid json}\n```"
	var obj map[string]interface{}
	_, err := result.Extract[map[string]interface{}](response)
	// err will be a json.UnmarshalError

	// No JSON found - returns trimmed input
	response = "Just plain text"
	data = result.ExtractJSON(response) // Returns "Just plain text"

# Thread Safety

All functions in this package are thread-safe. They operate on immutable input
strings and don't maintain any shared state, making them safe for concurrent use.

# Integration with AI Services

This package is designed to work with responses from various AI services:

Claude example:

	response, _ := claude.Complete(prompt)
	analysis, err := result.Extract[AnalysisResult](response.Content)

Google Gemini example:

	response, _ := gemini.Generate(prompt)
	summary, err := result.Extract[Summary](response.Text)

OpenAI example:

	response, _ := openai.CreateCompletion(prompt)
	data, err := result.Extract[ResponseData](response.Choices[0].Text)

# Performance Considerations

The package is optimized for typical AI response sizes:

  - Efficient string processing without regex
  - Single pass extraction for markdown blocks
  - Minimal memory allocations
  - No external dependencies beyond standard library

# Common Patterns

Working with structured AI responses:

	// Command AI to return specific JSON structure
	prompt := `Analyze this code and return JSON in this format:
	{
		"issues": [{"line": number, "message": "string", "severity": "string"}],
		"summary": "string"
	}`

	response := callAI(prompt)

	type CodeAnalysis struct {
		Issues []struct {
			Line     int    `json:"line"`
			Message  string `json:"message"`
			Severity string `json:"severity"`
		} `json:"issues"`
		Summary string `json:"summary"`
	}

	analysis, err := result.Extract[CodeAnalysis](response)
	if err != nil {
		// Handle extraction or parsing error
		return fmt.Errorf("failed to parse AI response: %w", err)
	}

	// Use the structured data
	for _, issue := range analysis.Issues {
		fmt.Printf("Line %d: %s (%s)\n", issue.Line, issue.Message, issue.Severity)
	}
*/
package result
