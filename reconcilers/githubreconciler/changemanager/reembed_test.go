/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/go-github/v88/github"
)

// newReembedServer serves GET /pulls/7 with the given labels and records PATCHes.
func newReembedServer(t *testing.T, labels ...string) (*github.Client, *[]string) {
	t.Helper()
	return newReembedServerBody(t, "PR body text", labels...)
}

// newReembedServerBody is newReembedServer with a configurable REST body.
func newReembedServerBody(t *testing.T, body string, labels ...string) (*github.Client, *[]string) {
	t.Helper()
	var patches []string
	prLabels := make([]*github.Label, 0, len(labels))
	for _, l := range labels {
		prLabels = append(prLabels, &github.Label{Name: new(l)})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, &github.PullRequest{Number: new(7), Body: new(body), Labels: prLabels,
			Head: &github.PullRequestBranch{SHA: new("live-head")}})
	})
	mux.HandleFunc("PATCH /api/v3/repos/test-owner/test-repo/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		patches = append(patches, r.URL.Path)
		writeJSON(t, w, &github.PullRequest{Number: new(7)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return client, &patches
}

func newReembedSession(t *testing.T, client *github.Client) *Session[testData] {
	t.Helper()
	s := newMarkerSession(client, 7)
	s.manager = &CM[testData]{identity: "test-bot", templateExecutor: mustTemplateExecutor(t)}
	return s
}

func TestReembedDataRefusesSkipLabel(t *testing.T) {
	client, patches := newReembedServer(t, "skip:test-bot")
	s := newReembedSession(t, client)

	if err := s.ReembedData(t.Context(), &testData{}); err == nil {
		t.Error("ReembedData: got error = nil, want skip-label error")
	}
	if len(*patches) != 0 {
		t.Errorf("PATCH calls: got = %v, want none", *patches)
	}
}

func TestReembedDataWritesWithoutSkipLabel(t *testing.T) {
	client, patches := newReembedServer(t, "automated pr")
	s := newReembedSession(t, client)

	if err := s.ReembedData(t.Context(), &testData{}); err != nil {
		t.Fatalf("ReembedData: got error = %v, want = nil", err)
	}
	if len(*patches) != 1 {
		t.Errorf("PATCH calls: got = %v, want 1", *patches)
	}
}

func TestReembedDataAtHead(t *testing.T) {
	tests := []struct {
		name      string
		head      string
		sessBody  string
		srvBody   string
		wantErr   bool
		wantPatch int
	}{
		{name: "head and body match", head: "live-head", sessBody: "PR body text", wantPatch: 1},
		{name: "CRLF server body matches LF session body", head: "live-head", sessBody: "a\nb", srvBody: "a\r\nb", wantPatch: 1},
		{name: "live head moved", head: "other-head", sessBody: "PR body text", wantErr: true},
		{name: "body edited since read", head: "live-head", sessBody: "older body", wantErr: true},
		{name: "no head", head: "", sessBody: "PR body text", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srvBody := tt.srvBody
			if srvBody == "" {
				srvBody = "PR body text"
			}
			client, patches := newReembedServerBody(t, srvBody)
			s := newReembedSession(t, client)
			s.prBody = tt.sessBody
			err := s.ReembedDataAtHead(t.Context(), &testData{}, tt.head)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReembedDataAtHead: got error = %v, want error = %v", err, tt.wantErr)
			}
			if len(*patches) != tt.wantPatch {
				t.Errorf("PATCH calls: got = %d, want = %d", len(*patches), tt.wantPatch)
			}
		})
	}
}

func TestClearReadyForReviewNoOpWithoutLabel(t *testing.T) {
	// The server has no label endpoints, so any removal attempt would fail.
	client, _ := newReembedServer(t)
	s := newReembedSession(t, client)
	s.prLabels = []string{"automated pr"}

	if _, err := s.ClearReadyForReview(t.Context()); err != nil {
		t.Fatalf("ClearReadyForReview: got error = %v, want = nil", err)
	}
	if !slices.Equal(s.prLabels, []string{"automated pr"}) {
		t.Errorf("labels: got = %v, want unchanged", s.prLabels)
	}
}
