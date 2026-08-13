/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
)

// ExampleClient_ListTeamIssues demonstrates sweeping the open issues of a team,
// restricted to those whose title carries a given prefix.
func ExampleClient_ListTeamIssues() {
	client := linearreconciler.NewClientWithAPIKey("lin_api_...")

	issues, err := client.ListTeamIssues(context.Background(), "SEC", linearreconciler.ListTeamIssuesOptions{
		TitleContains: "[depthfirst]",
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, issue := range issues {
		fmt.Println(issue.Identifier)
	}
}

// ExampleIssue_HasLabel demonstrates checking whether an issue has a specific label.
func ExampleIssue_HasLabel() {
	issue := &linearreconciler.Issue{}
	issue.Labels.Nodes = []struct {
		Name string `json:"name"`
	}{
		{Name: "bug"},
		{Name: "priority:high"},
	}
	fmt.Println(issue.HasLabel("bug"))
	fmt.Println(issue.HasLabel("enhancement"))
	// Output:
	// true
	// false
}

// ExampleIssue_FindAttachment demonstrates finding an attachment by title.
func ExampleIssue_FindAttachment() {
	issue := &linearreconciler.Issue{}
	issue.Attachments.Nodes = []linearreconciler.Attachment{
		{ID: "att-1", Title: "reconciler_state", URL: "https://example.com/state.json"},
	}

	att := issue.FindAttachment("reconciler_state")
	if att != nil {
		fmt.Println(att.Title)
	}

	missing := issue.FindAttachment("nonexistent")
	fmt.Println(missing)
	// Output:
	// reconciler_state
	// <nil>
}

// ExampleIssue_HasUnprocessedComments demonstrates checking for unprocessed comments.
func ExampleIssue_HasUnprocessedComments() {
	botID := "bot-user-id"

	issue := &linearreconciler.Issue{}
	issue.Comments.Nodes = []linearreconciler.Comment{
		{ID: "c1", Body: "Please fix this.", User: linearreconciler.User{ID: "user-1"}},
		{ID: "c2", Body: "On it.", User: linearreconciler.User{ID: botID}},
		{ID: "c3", Body: "Still broken.", User: linearreconciler.User{ID: "user-1"}},
	}

	fmt.Println(issue.HasUnprocessedComments(botID))
	// Output:
	// true
}
