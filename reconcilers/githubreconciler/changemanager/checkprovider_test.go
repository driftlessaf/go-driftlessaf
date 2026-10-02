/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-github/v88/github"
)

func TestCheckProviderRerun(t *testing.T) {
	tests := []struct {
		name       string
		detailsURL string
		identifier string
		status     int
		wantErr    string
	}{{
		name:       "valid actions URL",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/222",
		status:     http.StatusCreated,
	}, {
		name:       "actions URL with query params",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/222?extra=1",
		status:     http.StatusCreated,
	}, {
		name:       "actions job rerun API failure",
		detailsURL: "https://github.com/owner/repo/actions/runs/111/job/333",
		status:     http.StatusForbidden,
		wantErr:    "rerun job 333",
	}, {
		// Non-Actions details URL (e.g. a reconciler's Cloud Logging link):
		// re-requested by check run ID.
		name:       "non-actions URL re-requests by check run ID",
		detailsURL: "https://console.cloud.google.com/logs/query;query=foo",
		identifier: "83331956684",
		status:     http.StatusCreated,
	}, {
		name:       "empty URL re-requests by check run ID",
		detailsURL: "",
		identifier: "555",
		status:     http.StatusCreated,
	}, {
		name:       "non-actions URL with non-numeric identifier",
		detailsURL: "https://github.com/owner/repo/pull/123",
		identifier: "not-a-number",
		wantErr:    "parse check run ID from identifier",
	}, {
		name:       "re-request API failure",
		detailsURL: "",
		identifier: "777",
		status:     http.StatusForbidden,
		wantErr:    "re-request check run 777",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			f := callbacks.Finding{DetailsURL: tt.detailsURL, Identifier: tt.identifier}
			err = matchCheckProvider([]CheckProvider{githubActions{}}, f).Rerun(t.Context(), client, "owner", "repo", f)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("error: got = nil, wanted containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error: got = %q, wanted containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCheckProviderRerunJobIDRouting(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	f := callbacks.Finding{DetailsURL: "https://github.com/myorg/myrepo/actions/runs/100/job/999"}
	if err := matchCheckProvider([]CheckProvider{githubActions{}}, f).Rerun(t.Context(), client, "myorg", "myrepo", f); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := "/api/v3/repos/myorg/myrepo/actions/jobs/999/rerun"
	if gotPath != wantPath {
		t.Errorf("API path: got = %q, wanted = %q", gotPath, wantPath)
	}
}

// TestCheckProviderRerunRerequestRouting verifies a non-Actions finding routes to the
// Checks re-request endpoint, keyed on the check run ID in the Identifier.
func TestCheckProviderRerunRerequestRouting(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	f := callbacks.Finding{DetailsURL: "https://console.cloud.google.com/logs/query;query=foo", Identifier: "83331956684"}
	if err := matchCheckProvider([]CheckProvider{githubActions{}}, f).Rerun(t.Context(), client, "myorg", "myrepo", f); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := "/api/v3/repos/myorg/myrepo/check-runs/83331956684/rerequest"
	if gotPath != wantPath {
		t.Errorf("API path: got = %q, wanted = %q", gotPath, wantPath)
	}
}

// fakeProvider matches findings whose details URL has prefix and records
// the reruns it is asked for.
type fakeProvider struct {
	prefix string
	reruns []string
}

func (p *fakeProvider) Match(f callbacks.Finding) bool {
	return strings.HasPrefix(f.DetailsURL, p.prefix)
}

func (p *fakeProvider) Logs(context.Context, *github.Client, string, string, callbacks.Finding) (string, error) {
	return "provider log", nil
}

func (p *fakeProvider) Rerun(_ context.Context, _ *github.Client, _, _ string, f callbacks.Finding) error {
	p.reruns = append(p.reruns, f.Identifier)
	return nil
}

// TestCheckProviderDispatch verifies that providers are tried in order ahead
// of GitHub Actions, and that an unmatched finding falls back to its Details
// and a check run re-request.
func TestCheckProviderDispatch(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		detailsURL string
		wantLogs   string
		wantRerun  bool
		wantPath   string
	}{{
		name:       "matching provider serves logs and reruns",
		prefix:     "https://console.example.com/",
		detailsURL: "https://console.example.com/org/abc/runs/123?job=lint",
		wantLogs:   "provider log",
		wantRerun:  true,
	}, {
		name:       "unmatched finding serves details and re-requests",
		prefix:     "https://console.example.com/",
		detailsURL: "https://console.cloud.google.com/logs/query",
		wantLogs:   "the details",
		wantPath:   "/api/v3/repos/o/r/check-runs/42/rerequest",
	}, {
		name:       "provider is tried before GitHub Actions",
		prefix:     "https://github.com/",
		detailsURL: "https://github.com/o/r/actions/runs/1/job/2",
		wantLogs:   "provider log",
		wantRerun:  true,
	}, {
		name:       "GitHub Actions still handles its jobs",
		prefix:     "https://console.example.com/",
		detailsURL: "https://github.com/o/r/actions/runs/1/job/2",
		wantPath:   "/api/v3/repos/o/r/actions/jobs/2/rerun",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()
			client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}
			provider := &fakeProvider{prefix: tt.prefix}
			providers := []CheckProvider{provider, githubActions{}}
			f := callbacks.Finding{Identifier: "42", Details: "the details", DetailsURL: tt.detailsURL}

			if tt.wantLogs != "" {
				got, err := matchCheckProvider(providers, f).Logs(t.Context(), client, "o", "r", f)
				if err != nil {
					t.Fatalf("Logs: %v", err)
				}
				if got != tt.wantLogs {
					t.Errorf("logs: got = %q, want = %q", got, tt.wantLogs)
				}
			}
			if err := matchCheckProvider(providers, f).Rerun(t.Context(), client, "o", "r", f); err != nil {
				t.Fatalf("Rerun: %v", err)
			}
			if gotRerun := len(provider.reruns) > 0; gotRerun != tt.wantRerun {
				t.Errorf("provider rerun: got = %v, want = %v", gotRerun, tt.wantRerun)
			}
			if gotPath != tt.wantPath {
				t.Errorf("API path: got = %q, want = %q", gotPath, tt.wantPath)
			}
		})
	}
}

func TestNewAppendsGitHubActions(t *testing.T) {
	provider := &fakeProvider{prefix: "https://console.example.com/"}
	cm, err := New[testData]("test-bot",
		template.Must(template.New("title").Parse("update")),
		template.Must(template.New("body").Parse("update")),
		WithCheckProviders[testData](provider),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := []CheckProvider{provider, githubActions{}}
	if !slices.Equal(cm.checkProviders, want) {
		t.Errorf("checkProviders: got = %v, want = %v", cm.checkProviders, want)
	}
}
