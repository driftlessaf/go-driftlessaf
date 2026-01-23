/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool_test

import (
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// ExampleParam demonstrates extracting required parameters from a Gemini function call.
func ExampleParam() {
	// Simulate a function call from Gemini
	call := &genai.FunctionCall{
		ID:   "call_123",
		Name: "get_weather",
		Args: map[string]any{
			"location":    "San Francisco",
			"temperature": 72.5,
			"days":        7,
			"detailed":    true,
		},
	}

	// Extract string parameter
	location, errResp := googletool.Param[string](call, "location")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp.Response)
		return
	}

	// Extract float parameter
	temperature, errResp := googletool.Param[float64](call, "temperature")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp.Response)
		return
	}

	// Extract integer parameter (automatic conversion from float64)
	days, errResp := googletool.Param[int](call, "days")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp.Response)
		return
	}

	// Extract boolean parameter
	detailed, errResp := googletool.Param[bool](call, "detailed")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp.Response)
		return
	}

	fmt.Printf("Location: %s, Temperature: %.1f, Days: %d, Detailed: %v\n",
		location, temperature, days, detailed)

	// Output:
	// Location: San Francisco, Temperature: 72.5, Days: 7, Detailed: true
}

// ExampleParam_missingParameter demonstrates error handling for missing required parameters.
func ExampleParam_missingParameter() {
	call := &genai.FunctionCall{
		ID:   "call_456",
		Name: "read_file",
		Args: map[string]any{
			"encoding": "utf-8",
		},
	}

	// Try to extract a missing required parameter
	_, errResp := googletool.Param[string](call, "filename")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp.Response["error"])
	}

	// Output:
	// Error: filename parameter is required
}

// ExampleOptionalParam demonstrates extracting optional parameters with default values.
func ExampleOptionalParam() {
	call := &genai.FunctionCall{
		ID:   "call_789",
		Name: "search",
		Args: map[string]any{
			"query": "golang generics",
			"limit": 20,
		},
	}

	// Extract existing parameter (overrides default)
	query, _ := googletool.OptionalParam(call, "query", "")
	fmt.Printf("Query: %s\n", query)

	// Extract existing limit (overrides default)
	limit, _ := googletool.OptionalParam(call, "limit", 10)
	fmt.Printf("Limit: %d\n", limit)

	// Extract missing parameter (uses default)
	offset, _ := googletool.OptionalParam(call, "offset", 0)
	fmt.Printf("Offset: %d\n", offset)

	// Extract missing boolean (uses default)
	includeMetadata, _ := googletool.OptionalParam(call, "include_metadata", false)
	fmt.Printf("Include Metadata: %v\n", includeMetadata)

	// Output:
	// Query: golang generics
	// Limit: 20
	// Offset: 0
	// Include Metadata: false
}

// ExampleError demonstrates creating error responses for function calls.
func ExampleError() {
	call := &genai.FunctionCall{
		ID:   "call_error",
		Name: "process_data",
	}

	// Simple error
	errResp := googletool.Error(call, "Invalid data format")
	fmt.Printf("Error ID: %s\n", errResp.ID)
	fmt.Printf("Error message: %v\n", errResp.Response["error"])

	// Formatted error
	filename := "data.csv"
	line := 42
	errResp = googletool.Error(call, "Parse error in %s at line %d", filename, line)
	fmt.Printf("Formatted error: %v\n", errResp.Response["error"])

	// Output:
	// Error ID: call_error
	// Error message: Invalid data format
	// Formatted error: Parse error in data.csv at line 42
}

// ExampleErrorWithContext demonstrates creating error responses with additional context.
func ExampleErrorWithContext() {
	call := &genai.FunctionCall{
		ID:   "call_context",
		Name: "upload_file",
		Args: map[string]any{
			"filename": "large_file.zip",
		},
	}

	// Simulate an error condition
	err := errors.New("file size exceeds limit")

	// Create error response with context
	errResp := googletool.ErrorWithContext(call, err, map[string]any{
		"filename":    "large_file.zip",
		"size_mb":     156.7,
		"limit_mb":    100,
		"retry_after": 3600,
	})

	// The response includes both the error and context
	fmt.Printf("Error: %v\n", errResp.Response["error"])
	fmt.Printf("Size: %.1f MB\n", errResp.Response["size_mb"])
	fmt.Printf("Limit: %d MB\n", errResp.Response["limit_mb"])

	// Output:
	// Error: file size exceeds limit
	// Size: 156.7 MB
	// Limit: 100 MB
}

// Example_completeFunctionImplementation demonstrates a complete function implementation.
func Example_completeFunctionImplementation() {
	// Function to handle a database query function call
	handleDatabaseQuery := func(call *genai.FunctionCall) *genai.FunctionResponse {
		// Extract required query parameter
		query, errResp := googletool.Param[string](call, "query")
		if errResp != nil {
			return errResp
		}

		// Extract optional parameters
		database, errResp := googletool.OptionalParam(call, "database", "default")
		if errResp != nil {
			return errResp
		}

		limit, errResp := googletool.OptionalParam(call, "limit", 100)
		if errResp != nil {
			return errResp
		}

		timeout, errResp := googletool.OptionalParam(call, "timeout_seconds", 30)
		if errResp != nil {
			return errResp
		}

		// Validate parameters
		if limit > 1000 {
			return googletool.Error(call, "Limit cannot exceed 1000, got %d", limit)
		}

		if timeout > 300 {
			return googletool.Error(call, "Timeout cannot exceed 300 seconds, got %d", timeout)
		}

		// Simulate query execution (in real code, you'd execute the actual query)
		if query == "SELECT * FROM invalid_table" {
			return googletool.ErrorWithContext(
				call,
				errors.New("table not found"),
				map[string]any{
					"query":    query,
					"database": database,
					"tables":   []string{"users", "orders", "products"},
				},
			)
		}

		// Return successful response
		return &genai.FunctionResponse{
			ID:   call.ID,
			Name: call.Name,
			Response: map[string]any{
				"rows": []map[string]any{
					{"id": 1, "name": "Alice", "email": "alice@example.com"},
					{"id": 2, "name": "Bob", "email": "bob@example.com"},
				},
				"row_count":      2,
				"execution_time": 0.125,
				"database":       database,
				"limit":          limit,
			},
		}
	}

	// Simulate a function call
	call := &genai.FunctionCall{
		ID:   "call_db_query",
		Name: "database_query",
		Args: map[string]any{
			"query":    "SELECT id, name, email FROM users LIMIT 2",
			"database": "production",
			"limit":    10,
		},
	}

	// Handle the function call
	response := handleDatabaseQuery(call)
	fmt.Printf("Response ID: %s\n", response.ID)
	fmt.Printf("Row count: %v\n", response.Response["row_count"])
	fmt.Printf("Database: %v\n", response.Response["database"])

	// Output:
	// Response ID: call_db_query
	// Row count: 2
	// Database: production
}

// ExampleParam_typeConversions demonstrates automatic type conversions for numeric types.
func ExampleParam_typeConversions() {
	// Gemini sends all numbers as float64 in JSON
	call := &genai.FunctionCall{
		ID:   "call_numbers",
		Name: "process_numbers",
		Args: map[string]any{
			"count":     42.0,    // JSON number (float64)
			"threshold": 99.5,    // Actual float
			"user_id":   12345.0, // Large integer as float64
		},
	}

	// Extract as different integer types
	count, _ := googletool.Param[int](call, "count")
	fmt.Printf("Count as int: %d\n", count)

	countInt32, _ := googletool.Param[int32](call, "count")
	fmt.Printf("Count as int32: %d\n", countInt32)

	userID, _ := googletool.Param[int64](call, "user_id")
	fmt.Printf("User ID as int64: %d\n", userID)

	// Extract as float64
	threshold, _ := googletool.Param[float64](call, "threshold")
	fmt.Printf("Threshold as float64: %.1f\n", threshold)

	// Output:
	// Count as int: 42
	// Count as int32: 42
	// User ID as int64: 12345
	// Threshold as float64: 99.5
}
