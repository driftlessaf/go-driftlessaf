/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

func TestLocalRunnerKeepsAuthenticatedReconcilerConstruction(t *testing.T) {
	calls := 0
	middlewareCalls := 0
	var cache *ClientCache
	err := Main(t.Context(), func(_ context.Context, identity string, cc *ClientCache, _ struct{}) (ReconcilerFunc, error) {
		if identity != "fixture" {
			t.Errorf("identity = %q", identity)
		}
		if cc.wrapTransport == nil {
			t.Fatal("conditional transport was lost")
		}
		cache = cc
		return func(_ context.Context, res *Resource, _ *github.Client) error {
			calls++
			if res.Repo != "repo" {
				t.Errorf("parsed repo = %q", res.Repo)
			}
			return workqueue.RequeueNotBefore(time.Minute)
		}, nil
	}, WithIdentity("fixture"), WithConditionalRequests(), withReconcilerOptions(WithOrgScopedCredentials()),
		WithTokenSourceFuncFactory(func(string) TokenSourceFunc {
			return func(_ context.Context, org, repo string) (oauth2.TokenSource, error) {
				if repo != "" {
					t.Errorf("credentials escaped org scope: %q/%q", org, repo)
				}
				return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture"}), nil
			}
		}), WithMiddleware(func(next ReconcilerFunc) ReconcilerFunc {
			return func(ctx context.Context, res *Resource, gh *github.Client) error {
				middlewareCalls++
				return next(ctx, res, gh)
			}
		}), WithRunner(func(ctx context.Context, rec workqueue.WorkqueueServiceServer) error {
			for _, org := range []string{"first", "second"} {
				resp, err := rec.Process(ctx, &workqueue.ProcessRequest{Key: "https://github.com/" + org + "/repo/pull/1"})
				if err != nil {
					return err
				}
				if resp.GetRequeueAfterSeconds() != 60 || !resp.GetRequeueFloor() {
					t.Errorf("retry floor lost: %v", resp)
				}
			}
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || middlewareCalls != 2 || cache.entries.len() != 2 {
		t.Fatalf("calls=%d, middleware=%d, isolated clients=%d", calls, middlewareCalls, cache.entries.len())
	}
}
