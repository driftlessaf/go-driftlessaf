/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler/metareconciler"
)

type baseCallbacks = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

type myResult struct {
	CommitMsg string
}

func (r *myResult) GetCommitMessage() string { return r.CommitMsg }

type myRequest struct {
	Title string
	Body  string
}

func (r *myRequest) Bind(prompt *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	return prompt.BindXML("Title", struct {
		XMLName struct{} `xml:"title"`
		Content string   `xml:",chardata"`
	}{Content: r.Title})
}

// Example_construction shows how to wire up a Linear metareconciler.
func Example_construction() {
	var (
		cm            *changemanager.CM[metareconciler.PRData[*myRequest]]
		cloneMeta     *clonemanager.Meta
		agent         metaagent.Agent[*myRequest, *myResult, baseCallbacks]
		linearClient  *linearreconciler.Client
		githubClients *githubreconciler.ClientCache
	)

	rec := metareconciler.New(
		"my-bot",
		cm,
		cloneMeta,
		[]string{"my-bot"},
		agent,
		func(_ context.Context, issue *linearreconciler.Issue, _ *changemanager.Session[metareconciler.PRData[*myRequest]]) (*myRequest, error) {
			return &myRequest{Title: issue.Title, Body: issue.Description}, nil
		},
		func(_ context.Context, _ *changemanager.Session[metareconciler.PRData[*myRequest]], _ *clonemanager.Lease) (baseCallbacks, error) {
			return toolcall.NewFindingTools(
				toolcall.NewWorktreeTools(toolcall.EmptyTools{}, callbacks.WorktreeCallbacks{}),
				callbacks.FindingCallbacks{},
			), nil
		},
		linearClient,
		githubClients,
	)

	_ = rec
	fmt.Println("Reconciler created")
	// Output:
	// Reconciler created
}

// Example_withRequiredLabel shows filtering by label.
func Example_withRequiredLabel() {
	var (
		cm            *changemanager.CM[metareconciler.PRData[*myRequest]]
		cloneMeta     *clonemanager.Meta
		agent         metaagent.Agent[*myRequest, *myResult, baseCallbacks]
		linearClient  *linearreconciler.Client
		githubClients *githubreconciler.ClientCache
	)

	identity := "my-bot"
	rec := metareconciler.New(
		identity,
		cm,
		cloneMeta,
		[]string{},
		agent,
		func(_ context.Context, issue *linearreconciler.Issue, _ *changemanager.Session[metareconciler.PRData[*myRequest]]) (*myRequest, error) {
			return &myRequest{Title: issue.Title}, nil
		},
		func(_ context.Context, _ *changemanager.Session[metareconciler.PRData[*myRequest]], _ *clonemanager.Lease) (baseCallbacks, error) {
			return toolcall.NewFindingTools(
				toolcall.NewWorktreeTools(toolcall.EmptyTools{}, callbacks.WorktreeCallbacks{}),
				callbacks.FindingCallbacks{},
			), nil
		},
		linearClient,
		githubClients,
		metareconciler.WithRequiredLabel(fmt.Sprintf("%s/managed", identity)),
	)

	_ = rec
	fmt.Println("Reconciler created with label filter")
	// Output:
	// Reconciler created with label filter
}
