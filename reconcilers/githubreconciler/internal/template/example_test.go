/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package template_test

import (
	"fmt"
	gotemplate "text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
)

// PRData represents metadata for a pull request.
type PRData struct {
	SourceBranch string
	TargetBranch string
	Version      string
}

func ExampleNew() {
	// Create a new Template instance for PR data
	tmpl, err := template.New[PRData]("my-bot", "-pr-data", "PR")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("Template created for identity: my-bot\n")
	_ = tmpl
	// Output: Template created for identity: my-bot
}

func ExampleTemplate_Execute() {
	tmpl, err := template.New[PRData]("my-bot", "-pr-data", "PR")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// Create a Go template for PR titles
	goTmpl := gotemplate.Must(gotemplate.New("title").Parse("Update {{.SourceBranch}} to {{.Version}}"))

	data := &PRData{
		SourceBranch: "main",
		TargetBranch: "release",
		Version:      "v1.2.3",
	}

	result, err := tmpl.Execute(goTmpl, data)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(result)
	// Output: Update main to v1.2.3
}

func ExampleTemplate_Embed() {
	tmpl, err := template.New[PRData]("my-bot", "-pr-data", "PR")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	data := &PRData{
		SourceBranch: "feature-branch",
		TargetBranch: "main",
		Version:      "v2.0.0",
	}

	body, err := tmpl.Embed("This PR updates dependencies.", data)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// The body now contains the original text plus embedded JSON data
	fmt.Println("Body contains embedded data:", len(body) > 30)
	// Output: Body contains embedded data: true
}

func ExampleTemplate_Extract() {
	tmpl, err := template.New[PRData]("my-bot", "-pr-data", "PR")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// First embed some data
	data := &PRData{
		SourceBranch: "feature-branch",
		TargetBranch: "main",
		Version:      "v2.0.0",
	}

	body, err := tmpl.Embed("This PR updates dependencies.", data)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// Now extract the data back
	extracted, err := tmpl.Extract(body)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Printf("Source: %s, Target: %s, Version: %s\n",
		extracted.SourceBranch, extracted.TargetBranch, extracted.Version)
	// Output: Source: feature-branch, Target: main, Version: v2.0.0
}
