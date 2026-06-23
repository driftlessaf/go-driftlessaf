/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import "github.com/google/go-github/v88/github"

// hasLabel checks if a pull request has a label with the given name.
func hasLabel(pr *github.PullRequest, labelName string) bool {
	for _, label := range pr.Labels {
		if label.GetName() == labelName {
			return true
		}
	}
	return false
}
