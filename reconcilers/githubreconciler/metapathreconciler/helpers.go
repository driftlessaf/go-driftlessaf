/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import "github.com/google/go-github/v88/github"

// worktreeRootSetter is an optional capability a request may implement to
// receive the absolute path of the checked-out worktree before the agent runs.
// It lets an agent-side post-execute step operate on the checkout. A request
// that does not implement it is unaffected, so this is fully backward
// compatible.
type worktreeRootSetter interface {
	SetWorktreeRoot(root string)
}

// baseCommitSetter is an optional capability a request may implement to receive
// the base commit the pull request branches from, before the agent runs. It
// lets an agent-side step scope work to the change (for example a linter run
// restricted to issues introduced after the base). A request that does not
// implement it is unaffected, so this is fully backward compatible.
type baseCommitSetter interface {
	SetBaseCommit(sha string)
}

// hasLabel checks if a pull request has a label with the given name.
func hasLabel(pr *github.PullRequest, labelName string) bool {
	for _, label := range pr.Labels {
		if label.GetName() == labelName {
			return true
		}
	}
	return false
}
