/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import "chainguard.dev/driftlessaf/agents/promptbuilder"

// PRContext contains the PR information for the agent prompt
//
// TODO: Consider adding a get_pr_files tool for richer context (file diffs, content)
// when the agent needs more information to generate accurate titles/descriptions.
// The current approach embeds filenames directly which is sufficient for most cases.
type PRContext struct {
	Owner        string   `xml:"owner"`
	Repo         string   `xml:"repo"`
	PRNumber     int      `xml:"pr_number"`
	Title        string   `xml:"title"`
	Body         string   `xml:"body"`
	Issues       []string `xml:"issues>issue"`
	ChangedFiles []string `xml:"changed_files>file"`
}

// Bind implements promptbuilder.Bindable using XML binding
func (c *PRContext) Bind(prompt *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return prompt.BindXML("pr_context", c)
}

// PRFixResult is the agent's response
type PRFixResult struct {
	Success      bool     `json:"success" jsonschema:"description=Whether the fixes were successfully applied"`
	FixesApplied []string `json:"fixes_applied" jsonschema:"description=List of fixes that were applied"`
	Reasoning    string   `json:"reasoning" jsonschema:"description=Explanation of the fixes or why they could not be applied"`
}
