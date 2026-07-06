/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"fmt"
	"net/url"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/chainguard-dev/clog"
)

// traceFooter renders the PR-body footer mapping the PR back to its agent
// trace. Without a configured dashboard (see WithTraceDashboard) it is the
// plain "Trace-ID: <id>" line. With one, the trace ID links to the
// dashboard's trace view and, when the reconcile's execution context carries
// a reconciler key, a second link lists every agent run for this PR — so a
// reviewer can read the full run history without querying BigQuery.
func (cm *CM[T]) traceFooter(ctx context.Context, traceID string) string {
	plain := fmt.Sprintf("\n\nTrace-ID: %s", traceID)
	if cm.traceDashboardURL == "" {
		return plain
	}

	traceURL, err := urlWithParam(cm.traceDashboardURL, "trace", traceID)
	if err != nil {
		clog.WarnContextf(ctx, "Invalid trace dashboard URL %q: %v", cm.traceDashboardURL, err)
		return plain
	}
	footer := fmt.Sprintf("\n\nTrace-ID: [%s](%s)", traceID, traceURL)

	if key := agenttrace.GetExecutionContext(ctx).ReconcilerKey; key != "" {
		// The base URL parsed above, so this cannot fail.
		allRunsURL, _ := urlWithParam(cm.traceDashboardURL, "reconcile", key)
		footer += fmt.Sprintf(" · [all agent runs for this PR](%s)", allRunsURL)
	}
	return footer
}

// urlWithParam returns base with the query parameter set, preserving any
// parameters already on base (e.g. a pinned "?env=staging").
func urlWithParam(base, key, value string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
