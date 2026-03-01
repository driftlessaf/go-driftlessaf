/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package template provides template execution and data embedding/extraction
// capabilities for GitHub reconcilers.
//
// # Overview
//
// This package enables reconcilers to embed structured data within GitHub
// entities (issues, pull requests) using HTML comments. The embedded data
// can later be extracted to restore state or track metadata across
// reconciliation cycles.
//
// # Features
//
//   - Generic type support for embedding any JSON-serializable data
//   - Template execution with Go's text/template
//   - Data embedding using HTML comment markers
//   - Data extraction with regex-based parsing
//   - Configurable identity and marker suffixes for multi-bot environments
//
// # Usage
//
// Create a Template instance with an identity, marker suffix, and entity type:
//
//	type PRData struct {
//	    SourceBranch string
//	    TargetBranch string
//	}
//
//	tmpl, err := template.New[PRData]("my-bot", "-pr-data", "PR")
//	if err != nil {
//	    return err
//	}
//
// Embed data in a body:
//
//	data := &PRData{SourceBranch: "feature", TargetBranch: "main"}
//	body, err := tmpl.Embed("Original PR description", data)
//
// Extract data from a body:
//
//	extracted, err := tmpl.Extract(body)
//
// Execute a Go template:
//
//	goTmpl := template.Must(template.New("title").Parse("Update {{.SourceBranch}}"))
//	title, err := tmpl.Execute(goTmpl, data)
//
// # Integration
//
// This package is designed for use with GitHub reconcilers that need to
// persist state across API calls. The HTML comment format is invisible
// when rendered in GitHub's UI but preserves data for extraction.
//
// # Thread Safety
//
// Template instances are safe for concurrent use after creation.
package template
