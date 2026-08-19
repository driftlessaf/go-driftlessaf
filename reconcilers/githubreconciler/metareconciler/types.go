/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/google/go-github/v88/github"
)

// Result is implemented by all agent result types.
// The commit message is used when pushing changes to the repository.
type Result interface {
	GetCommitMessage() string
}

// RequestBuilder builds an agent request from an issue and session.
type RequestBuilder[Req any, Data any] func(context.Context, *github.Issue, *changemanager.Session[Data]) (Req, error)

// CallbacksBuilder builds agent callbacks from a session and lease.
type CallbacksBuilder[CB any, Data any] func(context.Context, *changemanager.Session[Data], *clonemanager.Lease) (CB, error)

// PRData is the data embedded in PR bodies for change detection.
// This is used by the changemanager to track state across reconciliations.
// It is parameterized by the request type so that request data can be
// incorporated into PR title and body templates. The Request field is
// excluded from JSON serialization and does not participate in state
// comparisons.
type PRData[Req any] struct {
	Identity      string   `json:"identity"`
	IssueURL      string   `json:"issue_url"`
	IssueNumber   int      `json:"issue_number"`
	IssueBodyHash [32]byte `json:"issue_body_hash"`
	Request       Req      `json:"-"`

	// ReasoningSummary is a truncated summary of the agent's extended-thinking
	// output for the run that produced this PR, populated by the reconciler
	// after the agent executes and empty when the run carried no reasoning.
	// Excluded from JSON so it never participates in change detection (it
	// varies run to run). Render it by appending [ReasoningSummarySnippet] to
	// the PR body template.
	ReasoningSummary string `json:"-"`

	// Headline says what the agent changed, for the PR title. The reconciler
	// copies it from the agent result after the agent commits, through
	// WithPRRenderFromResult. It is empty when a bot does not set that option,
	// and empty when the agent did not run, so a title template that uses it
	// needs an {{else}} branch. IssueURL and IssueNumber name the issue that
	// started the run. Headline names the change that the agent made. It is
	// excluded from JSON, so it never takes part in change detection.
	Headline string `json:"-"`

	// VariantSummary is a short markdown note about the change, for the PR
	// body. The reconciler copies it from the agent result after the agent
	// commits, through WithPRRenderFromResult. It is empty when a bot does not
	// set that option, and empty when the agent did not run, so a body
	// template that uses it needs an {{if}} guard. It is excluded from JSON,
	// so it never takes part in change detection.
	VariantSummary string `json:"-"`
}

// PRRender holds the render-only PR fields that a bot gets from an agent
// result. The reconciler copies them onto PRData after the agent commits, so
// the PR title template and the PR body template can render them. See
// WithPRRenderFromResult.
type PRRender struct {
	// Headline fills PRData.Headline for the PR title.
	Headline string

	// VariantSummary fills PRData.VariantSummary for the PR body.
	VariantSummary string
}
