/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v75/github"
)

// Result is implemented by all agent result types.
// The commit message is used when pushing changes to the repository.
type Result interface {
	GetCommitMessage() string
}

// RequestBuilder builds an agent request from an issue and session.
type RequestBuilder[Req any, Data any] func(*github.Issue, *changemanager.Session[Data]) Req

// CallbacksBuilder builds agent callbacks from a worktree and session.
type CallbacksBuilder[CB any, Data any] func(*gogit.Worktree, *changemanager.Session[Data]) CB

// PRData is the data embedded in PR bodies for change detection.
// This is used by the changemanager to track state across reconciliations.
type PRData struct {
	Identity      string   `json:"identity"`
	IssueURL      string   `json:"issue_url"`
	IssueNumber   int      `json:"issue_number"`
	IssueBodyHash [32]byte `json:"issue_body_hash"`
}
