/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"github.com/google/go-github/v88/github"
	"github.com/shurcooL/githubv4"
)

// AutoMergeResult says whether EnableAutoMerge armed auto-merge and, if
// GitHub refused, which benign refusal it gave. The zero value is none of
// the results below; EnableAutoMerge returns it only with an error.
type AutoMergeResult int

const (
	// AutoMergeArmed means GitHub armed auto-merge, and will merge the pull
	// request once its merge requirements are met.
	AutoMergeArmed AutoMergeResult = iota + 1

	// AutoMergeRefusedClean means GitHub refused because the pull request is
	// in clean status: it is mergeable and every check passes.
	AutoMergeRefusedClean

	// AutoMergeRefusedUnstable means GitHub refused because the pull request
	// is in unstable status: it is mergeable, but a check that is not
	// required is not passing.
	AutoMergeRefusedUnstable
)

// String returns "armed", "clean" or "unstable".
func (r AutoMergeResult) String() string {
	switch r {
	case AutoMergeArmed:
		return "armed"
	case AutoMergeRefusedClean:
		return "clean"
	case AutoMergeRefusedUnstable:
		return "unstable"
	default:
		return fmt.Sprintf("AutoMergeResult(%d)", int(r))
	}
}

// EnableAutoMerge asks GitHub to merge the pull request whose GraphQL node ID
// is prID with method once its merge requirements are met.
//
// GitHub refuses to arm auto-merge on a pull request that is already
// mergeable, because auto-merge would have nothing to wait for. It gives one
// of two reasons, named after the pull request's mergeStateStatus:
//
//   - "clean status": the pull request is mergeable and every check passes.
//   - "unstable status": the pull request is mergeable, but a check that is
//     not required is not passing.
//
// EnableAutoMerge treats both refusals as benign and returns
// AutoMergeRefusedClean or AutoMergeRefusedUnstable with a nil error. Neither
// means the request was malformed or the caller lacks permission, and a
// caller that returned them as errors would retry a call that keeps failing
// until the pull request changes. Auto-merge is not armed after either
// refusal, though, so nothing merges the pull request unless the caller does:
// the caller chooses whether to leave it for a person or to merge it
// directly. Merging directly after AutoMergeRefusedUnstable merges a pull
// request with a check that is not passing, so a caller that merges directly
// after AutoMergeRefusedClean should handle AutoMergeRefusedUnstable
// separately.
//
// Every other refusal, such as a token without permission to merge, is
// returned as an error.
//
// GitHub reports the reason only in the error message, and the error's type
// (UNPROCESSABLE) is shared with other refusals, so EnableAutoMerge matches
// the message text.
func EnableAutoMerge(ctx context.Context, gh *github.Client, prID githubv4.ID, method githubv4.PullRequestMergeMethod) (AutoMergeResult, error) {
	var m struct {
		EnablePullRequestAutoMerge struct {
			PullRequest struct{ ID githubv4.ID }
		} `graphql:"enablePullRequestAutoMerge(input: $input)"`
	}
	input := githubv4.EnablePullRequestAutoMergeInput{PullRequestID: prID, MergeMethod: &method}
	err := graphqlclient.NewGraphQLClient(gh).Mutate(ctx, "EnableAutoMerge", &m, input, nil)
	switch {
	case err == nil:
		return AutoMergeArmed, nil
	case strings.Contains(err.Error(), "is in clean status"):
		return AutoMergeRefusedClean, nil
	case strings.Contains(err.Error(), "is in unstable status"):
		return AutoMergeRefusedUnstable, nil
	default:
		return 0, fmt.Errorf("enable auto-merge: %w", err)
	}
}
