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
}
