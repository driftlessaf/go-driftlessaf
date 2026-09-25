/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"

	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"github.com/google/go-github/v88/github"
)

// appAttribution returns a Middleware that stamps the App id — and, when
// lookup resolves one, the installation id — onto each reconcile's context,
// where httpmetrics' instrumented transport reads them into the app_id and
// installation_id labels on every github_rate_limit_* series the reconcile's
// API calls produce.
//
// This is what makes a shared installation budget attributable. The budget is
// granted per (App, installation) and every workload signing as the same App
// draws on one pool; without these labels every consumer's series carries
// app_id="" and an exhausted pool cannot be split by spender (ACID-688). The
// labels ride the reconcile context because go-github propagates it onto each
// request, and httpmetrics — the outermost transport — reads the request's
// own context; nothing needs to thread values through the client or the
// transport chain.
//
// The installation lookup is best-effort: attribution must never fail a
// reconcile, and an org the App is not installed on fails properly at token
// mint, with a real error. lookup is the App's cached resolver, primed by
// that same minting, so the steady-state cost here is a map read.
func appAttribution(appID int64, lookup func(ctx context.Context, org string) (int64, error)) Middleware {
	return func(next ReconcilerFunc) ReconcilerFunc {
		return func(ctx context.Context, res *Resource, gh *github.Client) error {
			ctx = httpmetrics.WithGitHubAppID(ctx, appID)
			if lookup != nil {
				if id, err := lookup(ctx, res.Owner); err == nil {
					ctx = httpmetrics.WithGitHubInstallationID(ctx, id)
				}
			}
			return next(ctx, res, gh)
		}
	}
}
