/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/terraform-infra-common/modules/github-bots/sdk"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// withMockedOctoToken swaps sdk.OctoTokenFunc for the duration of the test.
func withMockedOctoToken(t *testing.T, fn func(ctx context.Context, identity, org, repo string) (string, error)) {
	t.Helper()
	original := sdk.OctoTokenFunc
	t.Cleanup(func() { sdk.OctoTokenFunc = original })
	sdk.OctoTokenFunc = fn
}

func TestNewRepoTokenSource_PassesArguments(t *testing.T) {
	ctx := t.Context()
	wantIdentity := fmt.Sprintf("identity-%d", rand.Int64())
	wantOrg := fmt.Sprintf("org-%d", rand.Int64())
	wantRepo := fmt.Sprintf("repo-%d", rand.Int64())
	wantToken := fmt.Sprintf("token-%d", rand.Int64())

	var gotIdentity, gotOrg, gotRepo string
	withMockedOctoToken(t, func(_ context.Context, identity, org, repo string) (string, error) {
		gotIdentity, gotOrg, gotRepo = identity, org, repo
		return wantToken, nil
	})

	ts := NewRepoTokenSource(ctx, wantIdentity, wantOrg, wantRepo)
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != wantToken {
		t.Errorf("AccessToken: got = %q, want = %q", tok.AccessToken, wantToken)
	}
	if gotIdentity != wantIdentity || gotOrg != wantOrg || gotRepo != wantRepo {
		t.Errorf("args to OctoTokenFunc: got = (%q,%q,%q), want = (%q,%q,%q)",
			gotIdentity, gotOrg, gotRepo, wantIdentity, wantOrg, wantRepo)
	}
}

func TestNewOrgTokenSource_PassesEmptyRepo(t *testing.T) {
	ctx := t.Context()
	wantIdentity := fmt.Sprintf("identity-%d", rand.Int64())
	wantOrg := fmt.Sprintf("org-%d", rand.Int64())

	var gotRepo string
	withMockedOctoToken(t, func(_ context.Context, _, _, repo string) (string, error) {
		gotRepo = repo
		return "tok", nil
	})

	ts := NewOrgTokenSource(ctx, wantIdentity, wantOrg)
	if _, err := ts.Token(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotRepo != "" {
		t.Errorf("repo: got = %q, want empty string for org-scoped token", gotRepo)
	}
}

func TestTokenSource_NotFound_RequeuesWithDelay(t *testing.T) {
	// Octo STS returning NotFound typically means the org's GitHub App
	// installation quota is exhausted. The DAF wrapper must translate that
	// into a workqueue requeue so the reconciler backs off cleanly rather
	// than retrying tightly.
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
			withMockedOctoToken(t, func(_ context.Context, _, _, _ string) (string, error) {
				return "", status.Error(codes.NotFound, "installation not found")
			})

			var ts oauth2.TokenSource
			if tt.repo == "" {
				ts = NewOrgTokenSource(ctx, "id", tt.org)
			} else {
				ts = NewRepoTokenSource(ctx, "id", tt.org, tt.repo)
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

func TestTokenSource_NonNotFoundError_PassesThrough(t *testing.T) {
	ctx := t.Context()
	wantErr := fmt.Errorf("error-%d", rand.Int64())

	withMockedOctoToken(t, func(_ context.Context, _, _, _ string) (string, error) {
		return "", wantErr
	})

	ts := NewOrgTokenSource(ctx, "id", "org")
	tok, err := ts.Token()
	if err == nil {
		t.Fatal("expected error but got none")
	}
	if tok != nil {
		t.Errorf("token: got = %v, want = nil", tok)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error: got = %v, want = %v", err, wantErr)
	}
}

func TestTokenSource_ReuseCachesUnderlyingToken(t *testing.T) {
	// The DAF wrapper does not insert its own caching, but the underlying SDK
	// token source uses oauth2.ReuseTokenSource. This proves the wiring keeps
	// that caching intact (callers don't pay an Octo STS round-trip per call).
	ctx := t.Context()

	callCount := 0
	withMockedOctoToken(t, func(_ context.Context, _, _, _ string) (string, error) {
		callCount++
		return fmt.Sprintf("token-%d", callCount), nil
	})

	ts := NewOrgTokenSource(ctx, "id", "org")

	first, err := ts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := ts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if first.AccessToken != second.AccessToken {
		t.Errorf("AccessToken: got = %q, want cached %q", second.AccessToken, first.AccessToken)
	}
	if callCount != 1 {
		t.Errorf("OctoTokenFunc call count: got = %d, want = 1", callCount)
	}
}
