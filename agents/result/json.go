/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package result

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ExtractJSON extracts JSON content from a text response that may contain markdown code blocks.
// It looks for content between ```json and ``` markers, or returns the input trimmed if no markers are found.
func ExtractJSON(responseText string) string {
	// Search for the first instance of ```json on its own line and collect content until closing ```
	lines := strings.Split(responseText, "\n")
	var jsonBuffer bytes.Buffer
	inJSONBlock := false
	foundJSON := false

	for _, line := range lines {
		if !inJSONBlock && line == "```json" {
			inJSONBlock = true
			foundJSON = true
			continue
		}

		if inJSONBlock && line == "```" {
			// Found closing marker, we're done
			break
		}

		if inJSONBlock {
			if jsonBuffer.Len() > 0 {
				jsonBuffer.WriteString("\n")
			}
			jsonBuffer.WriteString(line)
		}
	}

	if foundJSON {
		if jsonBuffer.Len() == 0 {
			// Found ```json block but it was empty, return empty string
			// The caller should handle this as an error
			return ""
		}
		return strings.TrimSpace(jsonBuffer.String())
	}

	// Fallback: clean the response text - sometimes models add extra whitespace or markdown formatting
	responseText = strings.TrimSpace(responseText)

	// If the response is wrapped in markdown code blocks, extract the JSON
	if strings.HasPrefix(responseText, "```json") && strings.HasSuffix(responseText, "```") {
		responseText = strings.TrimPrefix(responseText, "```json")
		responseText = strings.TrimSuffix(responseText, "```")
		responseText = strings.TrimSpace(responseText)
	} else {
		// These do nothing if the values aren't there, so always do it.
		responseText = strings.TrimPrefix(responseText, "```")
		responseText = strings.TrimSuffix(responseText, "```")
		responseText = strings.TrimSpace(responseText)
	}

	return responseText
}

// Extract extracts JSON content from a text response and unmarshals it into the provided type.
// It combines ExtractJSON with json.Unmarshal for convenience.
func Extract[T any](responseText string) (T, error) {
	var result T

	// Extract the JSON content
	jsonContent := ExtractJSON(responseText)

	// Unmarshal into the result type
	if err := json.Unmarshal([]byte(jsonContent), &result); err != nil {
		return result, err
	}

	return result, nil
}
