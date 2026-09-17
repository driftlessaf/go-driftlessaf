/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v88/github"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	internaltemplate "chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
)

const (
	skipIdentity = "example-check"
	skipOwner    = "acme"
	skipRepo     = "widgets"
	skipSHA      = "b4d1dea5b4d1dea5b4d1dea5b4d1dea5b4d1dea5"
	skipRunID    = int64(90210)
)

// AnnotatedDetails carries an annotation, exercising the case the skip must
// never take.
type AnnotatedDetails struct {
	Message string `json:"message"`
}

func (d AnnotatedDetails) Annotations() []*github.CheckRunAnnotation {
	return []*github.CheckRunAnnotation{{
		Path:            new("config/pipeline.yaml"),
		StartLine:       new(1),
		EndLine:         new(1),
		AnnotationLevel: new("failure"),
		Message:         new(d.Message),
	}}
}

// skipServer serves the two endpoints one SetActualState round touches and
// records which write, if any, reached GitHub.
type skipServer struct {
	patches int
	posts   int
	// live is the check run the listing endpoint returns; nil means the
	// commit carries none, which drives SetActualState down the create path.
	live *github.CheckRun
}

func (s *skipServer) start(t *testing.T) *github.Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("GET /api/v3/repos/%s/%s/commits/{ref}/check-runs", skipOwner, skipRepo),
		func(w http.ResponseWriter, _ *http.Request) {
			runs := []*github.CheckRun{}
			if s.live != nil {
				runs = append(runs, s.live)
			}
			writeSkipJSON(t, w, map[string]any{"total_count": len(runs), "check_runs": runs})
		})
	mux.HandleFunc(fmt.Sprintf("PATCH /api/v3/repos/%s/%s/check-runs/{id}", skipOwner, skipRepo),
		func(w http.ResponseWriter, _ *http.Request) {
			s.patches++
			writeSkipJSON(t, w, map[string]any{"id": skipRunID})
		})
	mux.HandleFunc(fmt.Sprintf("POST /api/v3/repos/%s/%s/check-runs", skipOwner, skipRepo),
		func(w http.ResponseWriter, _ *http.Request) {
			s.posts++
			writeSkipJSON(t, w, map[string]any{"id": skipRunID})
		})
	mux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := github.NewClient(
		github.WithHTTPClient(srv.Client()),
		github.WithEnterpriseURLs(srv.URL, srv.URL),
	)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return client
}

func writeSkipJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

// skipManager builds a manager with the details link suppressed, so the
// comparison does not depend on the Cloud Logging URL the default builder
// derives from environment the test does not have.
func skipManager[T any](t *testing.T, skipUnchanged bool) *StatusManager[T] {
	t.Helper()
	executor, err := internaltemplate.New[Status[T]](skipIdentity, "-status", "status")
	if err != nil {
		t.Fatalf("creating template executor: %v", err)
	}
	return &StatusManager[T]{
		identity:         skipIdentity,
		skipUnchanged:    skipUnchanged,
		detailsURLFunc:   func(*githubreconciler.Resource, string) string { return "" },
		templateExecutor: executor,
	}
}

// liveRun renders the check run GitHub would be showing after the manager
// wrote title/status. Built through buildCheckRunOutput rather than by hand:
// the summary carries an embedded status marker, and a hand-rolled
// approximation of it would make this test pass on a summary the real writer
// would never produce.
func liveRun[T any](t *testing.T, sm *StatusManager[T], title string, status *Status[T]) *github.CheckRun {
	t.Helper()
	status.ObservedGeneration = skipSHA
	summary, err := sm.buildCheckRunOutput(status)
	if err != nil {
		t.Fatalf("buildCheckRunOutput: %v", err)
	}
	run := &github.CheckRun{
		ID:     new(skipRunID),
		Name:   new(skipIdentity),
		Status: new(status.Status),
		Output: &github.CheckRunOutput{Title: new(title), Summary: new(summary)},
	}
	if status.Conclusion != "" {
		run.Conclusion = new(status.Conclusion)
	}
	return run
}

// TestSkipUnchangedUpdates is the whole contract: a write that would leave the
// check run exactly as it is costs no call, and every other write still does.
func TestSkipUnchangedUpdates(t *testing.T) {
	// The state already on the check run in every case below.
	const liveTitle = "3/7 check(s) complete"
	liveStatus := func() *Status[TestDetails] {
		return &Status[TestDetails]{
			Status:  "in_progress",
			Details: TestDetails{Message: "jobs/4f2c", Count: 3},
		}
	}

	for _, tc := range []struct {
		name          string
		skipUnchanged bool
		title         string
		status        *Status[TestDetails]
		wantPatches   int
	}{{
		name:          "identical state writes nothing",
		skipUnchanged: true,
		title:         liveTitle,
		status:        liveStatus(),
		wantPatches:   0,
	}, {
		// The negative control: without the option the same call writes, which
		// is what proves the case above is the option's doing and not an
		// artifact of the fake.
		name:          "identical state still writes when the option is off",
		skipUnchanged: false,
		title:         liveTitle,
		status:        liveStatus(),
		wantPatches:   1,
	}, {
		// The poll that matters: a run advancing a job moves only the title,
		// since the embedded Details carry the run name alone.
		name:          "a changed title writes",
		skipUnchanged: true,
		title:         "4/7 check(s) complete",
		status:        liveStatus(),
		wantPatches:   1,
	}, {
		name:          "a changed summary writes",
		skipUnchanged: true,
		title:         liveTitle,
		status: &Status[TestDetails]{
			Status:  "in_progress",
			Details: TestDetails{Message: "jobs/4f2c", Count: 4},
		},
		wantPatches: 1,
	}, {
		name:          "a changed status writes",
		skipUnchanged: true,
		title:         liveTitle,
		status: &Status[TestDetails]{
			Status:  "completed",
			Details: TestDetails{Message: "jobs/4f2c", Count: 3},
		},
		wantPatches: 1,
	}, {
		name:          "a newly set conclusion writes",
		skipUnchanged: true,
		title:         liveTitle,
		status: &Status[TestDetails]{
			Status:     "in_progress",
			Conclusion: "success",
			Details:    TestDetails{Message: "jobs/4f2c", Count: 3},
		},
		wantPatches: 1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			sm := skipManager[TestDetails](t, tc.skipUnchanged)
			srv := &skipServer{live: liveRun(t, sm, liveTitle, liveStatus())}
			client := srv.start(t)

			session := sm.NewSession(client, &githubreconciler.Resource{
				Owner: skipOwner, Repo: skipRepo, Type: githubreconciler.ResourceTypePullRequest,
			}, skipSHA)

			if _, err := session.ObservedState(t.Context()); err != nil {
				t.Fatalf("ObservedState: %v", err)
			}
			if err := session.SetActualState(t.Context(), tc.title, tc.status); err != nil {
				t.Fatalf("SetActualState: %v", err)
			}

			if srv.patches != tc.wantPatches {
				t.Errorf("check-run updates: got = %d, want = %d", srv.patches, tc.wantPatches)
			}
			if srv.posts != 0 {
				t.Errorf("check-run creates: got = %d, want = 0 (a run was already observed)", srv.posts)
			}
		})
	}
}

// TestSkipUnchangedNeverSkipsAnnotations pins the carve-out. GitHub appends
// annotations rather than replacing them, and a check run's listing carries
// only their count, so the session cannot tell equal counts from equal
// annotations. A reconciler that publishes them keeps writing every time.
func TestSkipUnchangedNeverSkipsAnnotations(t *testing.T) {
	sm := skipManager[AnnotatedDetails](t, true)
	status := &Status[AnnotatedDetails]{
		Status:     "completed",
		Conclusion: "failure",
		Details:    AnnotatedDetails{Message: "pipeline.yaml declares no steps"},
	}
	const title = "Invalid config: config/pipeline.yaml"

	// The live run is byte-identical to what this write would set; only the
	// annotations make it unskippable.
	srv := &skipServer{live: liveRun(t, sm, title, &Status[AnnotatedDetails]{
		Status:     status.Status,
		Conclusion: status.Conclusion,
		Details:    status.Details,
	})}
	client := srv.start(t)

	session := sm.NewSession(client, &githubreconciler.Resource{
		Owner: skipOwner, Repo: skipRepo, Type: githubreconciler.ResourceTypePullRequest,
	}, skipSHA)

	if _, err := session.ObservedState(t.Context()); err != nil {
		t.Fatalf("ObservedState: %v", err)
	}
	if err := session.SetActualState(t.Context(), title, status); err != nil {
		t.Fatalf("SetActualState: %v", err)
	}

	if srv.patches != 1 {
		t.Errorf("check-run updates: got = %d, want = 1 (an update carrying annotations is never skipped)", srv.patches)
	}
}

// TestSkipUnchangedCreatesWithoutAnObservedRun pins that only the update path
// is ever skipped: with no check run at the commit there is nothing to compare
// against, so the create proceeds.
func TestSkipUnchangedCreatesWithoutAnObservedRun(t *testing.T) {
	sm := skipManager[TestDetails](t, true)
	srv := &skipServer{live: nil}
	client := srv.start(t)

	session := sm.NewSession(client, &githubreconciler.Resource{
		Owner: skipOwner, Repo: skipRepo, Type: githubreconciler.ResourceTypePullRequest,
	}, skipSHA)

	observed, err := session.ObservedState(t.Context())
	if err != nil {
		t.Fatalf("ObservedState: %v", err)
	}
	if observed != nil {
		t.Fatalf("ObservedState: got = %+v, want = nil (the commit carries no check run)", observed)
	}
	if err := session.SetActualState(t.Context(), "Run accepted; waiting for checks to start",
		&Status[TestDetails]{Status: "queued", Details: TestDetails{Message: "jobs/4f2c"}}); err != nil {
		t.Fatalf("SetActualState: %v", err)
	}

	if srv.posts != 1 {
		t.Errorf("check-run creates: got = %d, want = 1", srv.posts)
	}
	if srv.patches != 0 {
		t.Errorf("check-run updates: got = %d, want = 0", srv.patches)
	}
}

// TestSkipUnchangedAfterAWriteInTheSameSession pins that a write refreshes
// what the session believes the check run shows. The first call writes (the
// state moved), the second repeats it and must not.
func TestSkipUnchangedAfterAWriteInTheSameSession(t *testing.T) {
	sm := skipManager[TestDetails](t, true)
	srv := &skipServer{live: liveRun(t, sm, "2/7 check(s) complete", &Status[TestDetails]{
		Status:  "in_progress",
		Details: TestDetails{Message: "jobs/4f2c", Count: 2},
	})}
	client := srv.start(t)

	session := sm.NewSession(client, &githubreconciler.Resource{
		Owner: skipOwner, Repo: skipRepo, Type: githubreconciler.ResourceTypePullRequest,
	}, skipSHA)

	if _, err := session.ObservedState(t.Context()); err != nil {
		t.Fatalf("ObservedState: %v", err)
	}

	advanced := func() *Status[TestDetails] {
		return &Status[TestDetails]{
			Status:  "in_progress",
			Details: TestDetails{Message: "jobs/4f2c", Count: 3},
		}
	}
	for range 3 {
		if err := session.SetActualState(t.Context(), "3/7 check(s) complete", advanced()); err != nil {
			t.Fatalf("SetActualState: %v", err)
		}
	}

	if srv.patches != 1 {
		t.Errorf("check-run updates: got = %d, want = 1 (the state moved once; the repeats change nothing)", srv.patches)
	}
}
