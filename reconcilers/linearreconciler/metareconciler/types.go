/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
)

// Result is implemented by all agent result types.
// The commit message is used when pushing changes to the repository.
type Result interface {
	GetCommitMessage() string
}

// RequestBuilder builds an agent request from a Linear issue and session.
type RequestBuilder[Req any, Data any] func(context.Context, *linearreconciler.Issue, *changemanager.Session[Data]) (Req, error)

// CallbacksBuilder builds agent callbacks from a session and lease.
type CallbacksBuilder[CB any, Data any] func(context.Context, *changemanager.Session[Data], *clonemanager.Lease) (CB, error)

// PRData is the data embedded in PR bodies for change detection.
// This is used by the changemanager to track state across reconciliations.
//
// Req is intentionally constrained as `any` here even though the surrounding
// Reconciler constrains it to promptbuilder.Bindable — PRData itself never
// calls methods on Req, so callers that share PRData outside the reconciler
// don't have to satisfy Bindable.
type PRData[Req any] struct {
	// Identity is the bot identity that owns this PR.
	Identity string `json:"identity"`
	// LinearIssueID is the Linear issue UUID. The keying field — used for
	// re-queueing because identifiers change when issues move teams.
	LinearIssueID string `json:"linear_issue_id"`
	// LinearIdentifier is the human identifier (e.g. "DEV-747"), used for
	// PR-body links only. Not for keying or dedup.
	LinearIdentifier string `json:"linear_identifier"`
	// DescriptionHash is the SHA-256 of the Linear issue description; used
	// to detect description edits and trigger agent re-runs.
	DescriptionHash [32]byte `json:"description_hash"`
	// Request is the agent input. Excluded from JSON because it's
	// reconstructed from the issue on each reconciliation; embedding it
	// would bloat the PR-body marker without adding value.
	Request Req `json:"-"`
}

// RepoTarget is the repo targeting information read from an upstream bot's
// state attachment. It tells the materializer which GitHub repository to
// clone and create PRs against.
type RepoTarget struct {
	Repo string `json:"repo"`           // "owner/repo" format
	Path string `json:"path,omitempty"` // optional subdirectory scope
}

// RepoTargetResolver resolves a repo target from a Linear issue.
// Used as a fallback when no upstream bot state attachment is available.
type RepoTargetResolver func(context.Context, *linearreconciler.Issue) (*RepoTarget, error)
