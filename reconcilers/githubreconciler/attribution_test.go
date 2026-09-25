/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubreconciler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"github.com/google/go-github/v88/github"
	"github.com/prometheus/client_golang/prometheus"
)

// rewriteHost redirects requests to the test server while leaving the URL the
// instrumentation sees untouched: httpmetrics only records GitHub rate-limit
// series for requests addressed to api.github.com, so the request must carry
// that host all the way down to this innermost hop.
type rewriteHost struct {
	inner  http.RoundTripper
	target string // host:port of the httptest server
}

func (rt rewriteHost) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = rt.target
	return rt.inner.RoundTrip(clone)
}

// gitHubRateLimitSeries returns the label map of the github_rate_limit series
// for org, or nil when no such series was recorded.
func gitHubRateLimitSeries(t *testing.T, org string) map[string]string {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "github_rate_limit" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["organization"] == org {
				return labels
			}
		}
	}
	return nil
}

// TestAppAttributionLabelsMetrics is the whole contract: a reconcile wrapped
// by the attribution middleware makes GitHub calls whose rate-limit series
// carry the App and installation ids, so a shared installation budget can be
// split by spender. It runs the real httpmetrics transport rather than
// reading the context back, because the context keys are private to
// httpmetrics — the metric labels are the only observable, and the one that
// matters.
func TestAppAttributionLabelsMetrics(t *testing.T) {
	for _, tc := range []struct {
		name string
		org  string
		// lookup resolves the installation; nil and failing lookups must
		// still stamp the App id and never fail the reconcile.
		lookup           func(context.Context, string) (int64, error)
		wantInstallation string
	}{{
		name:             "app and installation stamped",
		org:              "attr-org-stamped",
		lookup:           func(_ context.Context, _ string) (int64, error) { return 67890, nil },
		wantInstallation: "67890",
	}, {
		name:             "failed lookup stamps the app alone",
		org:              "attr-org-lookup-fails",
		lookup:           func(_ context.Context, _ string) (int64, error) { return 0, errors.New("no installation") },
		wantInstallation: "",
	}, {
		name:             "nil lookup stamps the app alone",
		org:              "attr-org-nil-lookup",
		lookup:           nil,
		wantInstallation: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-RateLimit-Resource", "core")
				w.Header().Set("X-RateLimit-Limit", "30000")
				w.Header().Set("X-RateLimit-Remaining", "29999")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			client := &http.Client{Transport: httpmetrics.WrapTransport(rewriteHost{
				inner:  srv.Client().Transport,
				target: srv.Listener.Addr().String(),
			})}

			rec := appAttribution(12345, tc.lookup)(func(ctx context.Context, res *Resource, _ *github.Client) error {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet,
					fmt.Sprintf("https://api.github.com/repos/%s/widgets", res.Owner), nil)
				if err != nil {
					return err
				}
				resp, err := client.Do(req)
				if err != nil {
					return err
				}
				return resp.Body.Close()
			})

			if err := rec(t.Context(), &Resource{Owner: tc.org, Repo: "widgets"}, nil); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			labels := gitHubRateLimitSeries(t, tc.org)
			if labels == nil {
				t.Fatalf("no github_rate_limit series recorded for organization %q", tc.org)
			}
			if got, want := labels["app_id"], "12345"; got != want {
				t.Errorf("app_id: got = %q, want = %q", got, want)
			}
			if got, want := labels["installation_id"], tc.wantInstallation; got != want {
				t.Errorf("installation_id: got = %q, want = %q", got, want)
			}
		})
	}
}
