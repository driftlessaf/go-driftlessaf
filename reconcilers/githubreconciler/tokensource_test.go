/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/sdk/octosts"
	"chainguard.dev/sdk/sts"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errExchanger is an sts.Exchanger whose Exchange always fails with err. It lets
// the requeue wrapper be tested through a real octosts token source, exercising
// the same error path production hits, without standing up a full STS server.
type errExchanger struct{ err error }

func (e *errExchanger) Exchange(context.Context, string, ...sts.ExchangerOption) (sts.TokenPair, error) {
	return sts.TokenPair{}, e.err
}

func (e *errExchanger) Refresh(context.Context, string, ...sts.ExchangerOption) (string, string, error) {
	return "", "", e.err
}

// octoSource returns a real octosts token source whose exchanges always fail
// with exchangeErr.
func octoSource(ctx context.Context, exchangeErr error) oauth2.TokenSource {
	base := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "base"})
	return octosts.NewTokenSourceContext(ctx, base, &errExchanger{err: exchangeErr})
}

func TestTokenSource_NotFound_RequeuesWithDelay(t *testing.T) {
	// Octo STS returning NotFound typically means the org's GitHub App
	// installation quota is exhausted. The DAF wrapper must translate that into
	// a workqueue requeue so the reconciler backs off cleanly rather than
	// retrying tightly.
	ctx := t.Context()

	tests := []struct {
		name string
		org  string
		repo string
	}{{
		name: "org-scoped",
		org:  fmt.Sprintf("org-%d", rand.Int64()),
	}, {
		name: "repo-scoped",
		org:  fmt.Sprintf("org-%d", rand.Int64()),
		repo: fmt.Sprintf("repo-%d", rand.Int64()),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := &requeueOnNotFound{
				ctx:   ctx,
				inner: octoSource(ctx, status.Error(codes.NotFound, "installation not found")),
				org:   tt.org,
				repo:  tt.repo,
			}

			tok, err := ts.Token()
			if err == nil {
				t.Fatal("expected error but got none")
			}
			if tok != nil {
				t.Errorf("token: got = %v, want = nil", tok)
			}
			delay, ok := workqueue.GetRequeueDelay(err)
			if !ok {
				t.Errorf("error type: got non-requeue error %v, want requeue error", err)
			} else if delay != 10*time.Minute {
				t.Errorf("requeue delay: got = %v, want = %v", delay, 10*time.Minute)
			}
		})
	}
}
