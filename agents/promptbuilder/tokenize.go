/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package promptbuilder

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// resolveFunc is a callback that provides a replacement for a binding name
type resolveFunc func(name string) (string, error)

// walkTemplate tokenizes the template and calls resolve for each binding
func walkTemplate(template string, resolve resolveFunc) (string, error) {
	var result strings.Builder

	for len(template) > 0 {
		// Find the next potential binding
		start := strings.Index(template, "{{")
		if start == -1 {
			// No more bindings, append the rest
			result.WriteString(template)
			break
		}

		// Append everything before the binding
		result.WriteString(template[:start])

		// Find the end of the binding
		end := strings.Index(template[start:], "}}")
		if end == -1 {
			// Malformed template, no closing }}
			return "", errors.New("unclosed binding: missing '}}'")
		}
		end += start + 2 // Adjust for the offset and include }}

		// Extract the binding name
		bindingText := template[start:end]
		bindingName := strings.TrimSpace(bindingText[2 : len(bindingText)-2])

		// Only process valid identifiers (alphanumeric + underscore)
		if isValidIdentifier(bindingName) {
			replacement, err := resolve(bindingName)
			if err != nil {
				return "", err
			}
			result.WriteString(replacement)
		} else {
			// Invalid identifier in binding
			return "", fmt.Errorf("invalid binding identifier %q", bindingName)
		}

		// Move past this binding
		template = template[end:]
	}

	return result.String(), nil
}

// isValidIdentifier checks if a string is a valid binding identifier
// Valid identifiers must start with a letter and contain only letters, digits, and underscores
func isValidIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	// First character must be a letter
	runes := []rune(s)
	if !unicode.IsLetter(runes[0]) {
		return false
	}
	// Remaining characters can be letters, digits, or underscores
	for _, r := range runes[1:] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}
