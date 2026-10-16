/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/runbudget"
	"github.com/google/go-github/v88/github"
)

// publishServer fakes the GitHub endpoints Upsert touches on the create and
// update paths and records the pull request payloads it received.
type publishServer struct {
	mu sync.Mutex

	freshPRDraft bool // draft state GET /pulls/{n} reports on the update path

	created *github.NewPullRequest // POST /pulls payload
	edited  *github.PullRequest    // PATCH /pulls/{n} payload
}

func newPublishServer(t *testing.T) (*github.Client, *publishServer) {
	t.Helper()
	rec := &publishServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/compare/{rest...}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, &github.CommitsComparison{Files: []*github.CommitFile{{Filename: new("README.md")}}})
	})
	mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		var payload github.NewPullRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding create payload: %v", err)
		}
		rec.mu.Lock()
		rec.created = &payload
		rec.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, &github.PullRequest{
			Number:  new(42),
			HTMLURL: new(fmt.Sprintf("https://%s/repos/test-owner/test-repo/pull/42", r.Host)),
		})
	})
	mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		rec.mu.Lock()
		draft := rec.freshPRDraft
		rec.mu.Unlock()
		writeJSON(t, w, &github.PullRequest{Number: new(99), NodeID: new("PR_node_99"), Draft: new(draft)})
	})
	mux.HandleFunc("PATCH /api/v3/repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var payload github.PullRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding edit payload: %v", err)
		}
		rec.mu.Lock()
		rec.edited = &payload
		rec.mu.Unlock()
		writeJSON(t, w, &github.PullRequest{Number: new(99)})
	})
	mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/labels", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []*github.Label{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return client, rec
}

// gqlRecorder records the GraphQL request bodies the session sends.
type gqlRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (g *gqlRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.bodies = append(g.bodies, string(b))
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"data":{}}`)
}

func (g *gqlRecorder) mutations(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, b := range g.bodies {
		if strings.Contains(b, name) {
			n++
		}
	}
	return n
}

func newPublishSession(t *testing.T, client *github.Client, gql *gqlRecorder, prNumber int) *Session[testData] {
	t.Helper()
	cm, err := New[testData]("test-bot",
		template.Must(template.New("title").Parse("{{.PackageName}}")),
		template.Must(template.New("body").Parse("Update {{.PackageName}} to {{.Version}}")))
	if err != nil {
		t.Fatalf("creating CM: %v", err)
	}
	s := &Session[testData]{
		manager:    cm,
		client:     client,
		owner:      "test-owner",
		repo:       "test-repo",
		branchName: "test-bot/issue-1",
		ref:        "main",
		prNumber:   prNumber,
	}
	if prNumber > 0 {
		s.prURL = fmt.Sprintf("https://example.invalid/pull/%d", prNumber)
	}
	if gql != nil {
		s.gqlClient = newTestGraphQLClient(t, gql)
	}
	return s
}

// TestUpsertPublishAsDraft proves that a session asked to publish as a draft
// opens a new pull request as a draft with the note in its body, converts an
// existing ready pull request to a draft, leaves an existing draft alone, and
// that a session not asked keeps the caller's draft flag.
func TestUpsertPublishAsDraft(t *testing.T) {
	const note = "This run stopped before the agent finished; the change is partial."
	tests := []struct {
		name          string
		prNumber      int
		freshPRDraft  bool
		publishDraft  bool
		callerDraft   bool
		wantDraft     bool // draft flag on the create payload
		wantNote      bool // note present in the body sent to GitHub
		wantConverted int  // convertPullRequestToDraft mutations
	}{
		{name: "create as draft with note", publishDraft: true, wantDraft: true, wantNote: true},
		{name: "create keeps caller flag when not asked", callerDraft: false, wantDraft: false},
		{name: "create honors caller draft when not asked", callerDraft: true, wantDraft: true},
		{name: "update converts a ready PR to draft", prNumber: 99, publishDraft: true, wantNote: true, wantConverted: 1},
		{name: "update leaves an existing draft alone", prNumber: 99, freshPRDraft: true, publishDraft: true, wantNote: true},
		{name: "update never converts when not asked", prNumber: 99, callerDraft: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newPublishServer(t)
			rec.freshPRDraft = tc.freshPRDraft
			gql := &gqlRecorder{}
			s := newPublishSession(t, client, gql, tc.prNumber)
			if tc.publishDraft {
				s.PublishAsDraft(note)
			}

			if _, err := s.Upsert(t.Context(), &testData{PackageName: "pkg", Version: "v1"}, tc.callerDraft, nil, func(context.Context, string) error { return nil }); err != nil {
				t.Fatalf("Upsert: %v", err)
			}

			var body string
			switch tc.prNumber {
			case 0:
				if rec.created == nil {
					t.Fatal("POST /pulls was not called")
				}
				if got := rec.created.GetDraft(); got != tc.wantDraft {
					t.Errorf("create draft flag: got = %t, want = %t", got, tc.wantDraft)
				}
				body = rec.created.GetBody()
			default:
				if rec.edited == nil {
					t.Fatal("PATCH /pulls/{n} was not called")
				}
				body = rec.edited.GetBody()
			}
			if got := strings.Contains(body, note); got != tc.wantNote {
				t.Errorf("note in body: got = %t, want = %t (body %q)", got, tc.wantNote, body)
			}
			if got := gql.mutations("convertPullRequestToDraft"); got != tc.wantConverted {
				t.Errorf("convertPullRequestToDraft mutations: got = %d, want = %d", got, tc.wantConverted)
			}
		})
	}
}

// TestUpsertPublishesAfterRequestEnds proves that, under a run budget, a
// request that ends after makeChanges returned cannot stop the pull request
// from being created, and that without a budget it still does.
func TestUpsertPublishesAfterRequestEnds(t *testing.T) {
	tests := []struct {
		name        string
		budget      bool
		wantCreated bool
		wantErr     error
	}{
		{name: "budget detaches publication", budget: true, wantCreated: true},
		{name: "no budget stays under the request", wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request, endRequest := context.WithCancel(t.Context())
			defer endRequest()
			ctx := request
			if tc.budget {
				ctx = runbudget.Start(request, runbudget.Budget{Total: time.Hour, Reserve: 8 * time.Minute}, time.Now())
			}
			client, rec := newPublishServer(t)
			s := newPublishSession(t, client, nil, 0)

			_, err := s.Upsert(ctx, &testData{PackageName: "pkg", Version: "v1"}, false, nil, func(context.Context, string) error {
				// The platform ends the request once the agent has pushed.
				endRequest()
				return nil
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Upsert: got err = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			if got := rec.created != nil; got != tc.wantCreated {
				t.Errorf("POST /pulls called: got = %t, want = %t", got, tc.wantCreated)
			}
		})
	}
}

// TestSalvagedDraftNoteFollowsDraftState proves that the note a stopped run
// published with survives a later run on the same pull request while it is
// still a draft, so a partial change goes on saying why it is a draft after
// the bot iterates on it, and that the note goes once someone marks the pull
// request ready for review, since the bot never marks it ready itself.
func TestSalvagedDraftNoteFollowsDraftState(t *testing.T) {
	const note = "This run stopped before the agent finished; the change is partial."
	tests := []struct {
		name     string
		isDraft  bool // draft state the later session reads
		wantNote bool // note rendered in the later run's body
	}{
		{name: "kept while the pull request is still a draft", isDraft: true, wantNote: true},
		{name: "dropped once the pull request is marked ready", isDraft: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The stopped run opens the draft.
			client, rec := newPublishServer(t)
			first := newPublishSession(t, client, &gqlRecorder{}, 0)
			first.PublishAsDraft(note)
			if _, err := first.Upsert(t.Context(), &testData{PackageName: "pkg", Version: "v1"}, false, nil, func(context.Context, string) error { return nil }); err != nil {
				t.Fatalf("first Upsert: %v", err)
			}
			salvagedBody := rec.created.GetBody()

			// A later run loads that pull request and iterates on it with new
			// data, without stopping early.
			gh, later := newLaterRunClient(t, salvagedBody, tc.isDraft)
			session, err := first.manager.NewSession(t.Context(), gh, &githubreconciler.Resource{
				Owner: "test-owner",
				Repo:  "test-repo",
				Ref:   "main",
				Path:  "packages/example.yaml",
				Type:  githubreconciler.ResourceTypePath,
			})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			if _, err := session.Upsert(t.Context(), &testData{PackageName: "pkg", Version: "v2"}, false, nil, func(context.Context, string) error { return nil }); err != nil {
				t.Fatalf("later Upsert: %v", err)
			}

			body := later.editedBody()
			if body == "" {
				t.Fatal("later run did not edit the pull request")
			}
			visible, _, _ := strings.Cut(body, "<!--test-bot-pr-data-->")
			if got := strings.Contains(visible, note); got != tc.wantNote {
				t.Errorf("note rendered in the later body: got = %t, want = %t (body %q)", got, tc.wantNote, body)
			}
			if !tc.wantNote && strings.Contains(body, note) {
				t.Errorf("note still recorded after the pull request was marked ready: %q", body)
			}
			if got := later.mutations("convertPullRequestToDraft"); got != 0 {
				t.Errorf("convertPullRequestToDraft mutations on a run that did not stop early: got = %d, want 0", got)
			}
		})
	}
}

// laterRun fakes the GitHub API a later session reads the salvaged pull
// request from and updates it through.
type laterRun struct {
	gqlRecorder
	edited string // body of the PATCH /pulls/{n} payload
}

func (l *laterRun) editedBody() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.edited
}

func newLaterRunClient(t *testing.T, body string, isDraft bool) (*github.Client, *laterRun) {
	t.Helper()
	rec := &laterRun{}
	prInfo, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": []any{map[string]any{
		"number": 99, "url": "https://github.com/test-owner/test-repo/pull/99", "body": body,
		"mergeable": "MERGEABLE", "isDraft": isDraft, "headRefOid": "abc123",
		"labels":        map[string]any{"nodes": []any{}},
		"commits":       map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"contexts": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": []any{}}}}}}},
		"assignees":     map[string]any{"nodes": []any{}},
		"reviewThreads": map[string]any{"nodes": []any{}},
		"reviews":       map[string]any{"nodes": []any{}},
	}}}}}})
	if err != nil {
		t.Fatalf("marshaling pull request info: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "pullRequests(") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(prInfo)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		rec.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /repos/test-owner/test-repo/compare/{rest...}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, &github.CommitsComparison{Files: []*github.CommitFile{{Filename: new("README.md")}}})
	})
	mux.HandleFunc("GET /repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, &github.PullRequest{Number: new(99), NodeID: new("PR_node_99"), Draft: new(isDraft)})
	})
	mux.HandleFunc("PATCH /repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		var payload github.PullRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding edit payload: %v", err)
		}
		rec.mu.Lock()
		rec.edited = payload.GetBody()
		rec.mu.Unlock()
		writeJSON(t, w, &github.PullRequest{Number: new(99)})
	})
	gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerRoundTripper{handler: mux}}))
	if err != nil {
		t.Fatalf("creating GitHub client: %v", err)
	}
	return gh, rec
}
