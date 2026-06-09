/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/terraform-infra-common/modules/github-bots/sdk"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// requeueOnNotFound wraps an oauth2.TokenSource so that gRPC NotFound errors
// from Octo STS are surfaced as workqueue.RequeueAfter. NotFound typically
// means the org's GitHub App installation quota has been exhausted; backing
// off lets the quota reset (or operators intervene) before we retry.
type requeueOnNotFound struct {
	ctx       context.Context
	inner     oauth2.TokenSource
	org, repo string
}

func (ts *requeueOnNotFound) Token() (*oauth2.Token, error) {
	tok, err := ts.inner.Token()
	if err == nil {
		return tok, nil
	}
	if status.Code(err) == codes.NotFound {
		scope := ts.org
		if ts.repo != "" {
			scope = ts.org + "/" + ts.repo
		}
		clog.ErrorContextf(ts.ctx, "Got NotFound error from Octo STS for %q: %v", scope, err)
		return nil, workqueue.RequeueAfter(10 * time.Minute)
	}
	return nil, err
}

// NewOrgTokenSource creates a new token source for org-scoped GitHub credentials.
// Token-source construction and caching come from the SDK primitive; this
// wrapper adds the workqueue-aware NotFound→RequeueAfter behaviour that DAF
// reconcilers depend on.
func NewOrgTokenSource(ctx context.Context, identity, org string) oauth2.TokenSource {
	return &requeueOnNotFound{
		ctx:   ctx,
		inner: sdk.NewOrgTokenSource(ctx, identity, org),
		org:   org,
	}
}

// NewRepoTokenSource creates a new token source for repo-scoped GitHub credentials.
// Token-source construction and caching come from the SDK primitive; this
// wrapper adds the workqueue-aware NotFound→RequeueAfter behaviour that DAF
// reconcilers depend on.
func NewRepoTokenSource(ctx context.Context, identity, org, repo string) oauth2.TokenSource {
	return &requeueOnNotFound{
		ctx:   ctx,
		inner: sdk.NewRepoTokenSource(ctx, identity, org, repo),
		org:   org,
		repo:  repo,
	}
}
