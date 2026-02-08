/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/metareconciler"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v75/github"
)

// baseCallbacks is the standard tool composition: Empty -> Worktree -> Finding
type baseCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

// MyResult implements metareconciler.Result.
type MyResult struct {
	CommitMsg string
	Summary   string
}

func (r *MyResult) GetCommitMessage() string {
	return r.CommitMsg
}

// MyRequest implements promptbuilder.Bindable.
type MyRequest struct {
	Title    string
	Body     string
	Findings []callbacks.Finding
}

func (r *MyRequest) Bind(prompt *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	prompt, err := prompt.BindXML("Title", struct {
		XMLName struct{} `xml:"title"`
		Content string   `xml:",chardata"`
	}{Content: r.Title})
	if err != nil {
		return nil, err
	}
	return prompt.BindXML("Body", struct {
		XMLName struct{} `xml:"body"`
		Content string   `xml:",chardata"`
	}{Content: r.Body})
}

// Example_reconcilerConstruction demonstrates how to construct a metareconciler.
// The reconciler orchestrates the flow of fetching issues, running an agent,
// and creating/updating PRs.
func Example_reconcilerConstruction() {
	// In practice, these would be created by the calling code
	var cm *changemanager.CM[metareconciler.PRData]
	var cloneMeta *clonemanager.Meta
	var agent metaagent.Agent[*MyRequest, *MyResult, baseCallbacks]

	// Create the reconciler with the agent and adapter functions
	rec := metareconciler.New(
		"my-bot",   // Bot identity for PR attribution
		cm,         // Change manager for PR operations
		cloneMeta,  // Clone manager for git operations
		[]string{}, // PR labels to apply
		agent,      // The agent that processes issues

		// Request builder: constructs the agent request from issue and session
		func(issue *github.Issue, session *changemanager.Session[metareconciler.PRData]) *MyRequest {
			return &MyRequest{
				Title:    issue.GetTitle(),
				Body:     issue.GetBody(),
				Findings: session.Findings(),
			}
		},

		// Callbacks builder: constructs agent callbacks from worktree and session
		func(wt *gogit.Worktree, session *changemanager.Session[metareconciler.PRData]) baseCallbacks {
			return toolcall.NewFindingTools(
				toolcall.NewWorktreeTools(toolcall.EmptyTools{}, clonemanager.WorktreeCallbacks(wt)),
				session.FindingCallbacks(),
			)
		},
	)

	_ = rec
	fmt.Println("Reconciler created")

	// Output:
	// Reconciler created
}

// Example_prData demonstrates the PRData type used for change detection.
// The PRData is embedded in PR bodies to track state across reconciliations.
func Example_prData() {
	// PRData tracks the relationship between issues and PRs
	data := metareconciler.PRData{
		Identity:    "my-bot",
		IssueURL:    "https://github.com/org/repo/issues/123",
		IssueNumber: 123,
		// IssueBodyHash is computed from the issue body to detect changes
	}

	fmt.Printf("Bot: %s, Issue: #%d\n", data.Identity, data.IssueNumber)

	// Output:
	// Bot: my-bot, Issue: #123
}

// Example_resultInterface demonstrates implementing the Result interface.
// The Result interface requires only GetCommitMessage() to provide the
// commit message for changes pushed to the repository.
func Example_resultInterface() {
	// Any type implementing GetCommitMessage() satisfies Result
	result := &MyResult{
		CommitMsg: "feat: implement new feature based on issue #123",
		Summary:   "Added new API endpoint with tests",
	}

	// The reconciler uses GetCommitMessage() when pushing changes
	var r metareconciler.Result = result
	fmt.Println(r.GetCommitMessage())

	// Output:
	// feat: implement new feature based on issue #123
}

// Example_withRequiredLabel demonstrates using the WithRequiredLabel option
// to filter issues during reconciliation. Only issues with the specified label
// will be processed; others are skipped.
func Example_withRequiredLabel() {
	// In practice, these would be created by the calling code
	var cm *changemanager.CM[metareconciler.PRData]
	var cloneMeta *clonemanager.Meta
	var agent metaagent.Agent[*MyRequest, *MyResult, baseCallbacks]

	identity := "my-bot"

	// Create the reconciler with label filtering.
	// Only issues with the "my-bot/managed" label will be processed.
	rec := metareconciler.New(
		identity,
		cm,
		cloneMeta,
		[]string{},
		agent,
		func(issue *github.Issue, session *changemanager.Session[metareconciler.PRData]) *MyRequest {
			return &MyRequest{
				Title: issue.GetTitle(),
				Body:  issue.GetBody(),
			}
		},
		func(wt *gogit.Worktree, session *changemanager.Session[metareconciler.PRData]) baseCallbacks {
			return toolcall.NewFindingTools(
				toolcall.NewWorktreeTools(toolcall.EmptyTools{}, callbacks.WorktreeCallbacks{}),
				callbacks.FindingCallbacks{},
			)
		},
		// Filter to only process issues managed by this identity
		metareconciler.WithRequiredLabel[*MyRequest, *MyResult, baseCallbacks](
			fmt.Sprintf("%s/managed", identity),
		),
	)

	_ = rec
	fmt.Println("Reconciler created with label filter")

	// Output:
	// Reconciler created with label filter
}
