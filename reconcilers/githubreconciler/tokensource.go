/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/sdk/octosts"
	"github.com/chainguard-dev/clog"
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
	initErr   error
	org, repo string
}

func (ts *requeueOnNotFound) scope() string {
	if ts.repo != "" {
		return ts.org + "/" + ts.repo
	}
	return ts.org
}

func (ts *requeueOnNotFound) Token() (*oauth2.Token, error) {
	// If the underlying Octo STS source could not be constructed, surface that
	// error lazily here so callers keep the simple oauth2.TokenSource contract.
	if ts.initErr != nil {
		return nil, fmt.Errorf("creating Octo STS token source for %q: %w", ts.scope(), ts.initErr)
	}
	tok, err := ts.inner.Token()
	if err == nil {
		return tok, nil
	}
	if status.Code(err) == codes.NotFound {
		clog.ErrorContextf(ts.ctx, "Got NotFound error from Octo STS for %q: %v", ts.scope(), err)
		return nil, workqueue.RequeueAfter(10 * time.Minute)
	}
	return nil, err
}

// NewOrgTokenSource creates a new token source for org-scoped GitHub credentials.
// Token-source construction and caching come from the Octo STS primitive; this
// wrapper adds the workqueue-aware NotFound→RequeueAfter behaviour that DAF
// reconcilers depend on. Any construction error is surfaced from the returned
// source's Token method so callers keep the simple oauth2.TokenSource contract.
func NewOrgTokenSource(ctx context.Context, identity, org string) oauth2.TokenSource {
	inner, err := octosts.NewTokenSourceFromValues(ctx, identity, org, "")
	return &requeueOnNotFound{
		ctx:     ctx,
		inner:   inner,
		initErr: err,
		org:     org,
	}
}

// NewRepoTokenSource creates a new token source for repo-scoped GitHub credentials.
// Token-source construction and caching come from the Octo STS primitive; this
// wrapper adds the workqueue-aware NotFound→RequeueAfter behaviour that DAF
// reconcilers depend on. Any construction error is surfaced from the returned
// source's Token method so callers keep the simple oauth2.TokenSource contract.
func NewRepoTokenSource(ctx context.Context, identity, org, repo string) oauth2.TokenSource {
	inner, err := octosts.NewTokenSourceFromValues(ctx, identity, org, repo)
	return &requeueOnNotFound{
		ctx:     ctx,
		inner:   inner,
		initErr: err,
		org:     org,
		repo:    repo,
	}
}
