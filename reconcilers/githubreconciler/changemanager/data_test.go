/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"github.com/google/go-github/v88/github"
)

type testData struct {
	PackageName string
	Version     string
	Commit      string
	// Nested exercises Extract on bodies whose JSON contains nested objects.
	Nested *testNested `json:",omitempty"`
}

type testNested struct {
	Field1 string
	Field2 int
}

func TestNew(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}/{{.Version}}"))
	bodyTmpl := template.Must(template.New("body").Parse("Update {{.PackageName}} to {{.Version}}"))

	tests := []struct {
		name          string
		identity      string
		titleTemplate *template.Template
		bodyTemplate  *template.Template
		wantErr       bool
		errContains   string
	}{{
		name:          "valid templates",
		identity:      "test-bot",
		titleTemplate: titleTmpl,
		bodyTemplate:  bodyTmpl,
		wantErr:       false,
	}, {
		name:          "nil title template",
		identity:      "test-bot",
		titleTemplate: nil,
		bodyTemplate:  bodyTmpl,
		wantErr:       true,
		errContains:   "titleTemplate cannot be nil",
	}, {
		name:          "nil body template",
		identity:      "test-bot",
		titleTemplate: titleTmpl,
		bodyTemplate:  nil,
		wantErr:       true,
		errContains:   "bodyTemplate cannot be nil",
	}, {
		name:          "both templates nil",
		identity:      "test-bot",
		titleTemplate: nil,
		bodyTemplate:  nil,
		wantErr:       true,
		errContains:   "titleTemplate cannot be nil",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm, err := New[testData](tt.identity, tt.titleTemplate, tt.bodyTemplate)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error: got = %v, wantErr = %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				if err == nil {
					t.Error("New() error: got = nil, want = non-nil error")
				} else if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("New() error message: got = %q, want to contain %q", err.Error(), tt.errContains)
				}
			} else {
				if cm == nil {
					t.Fatal("New() result: got = nil, want = non-nil CM")
					return
				}
				if cm.identity != tt.identity {
					t.Errorf("New() identity: got = %q, want = %q", cm.identity, tt.identity)
				}
			}
		})
	}
}

// TestExtractFromBody verifies the body-only Extract helper round-trips data
// embedded with the same template, including JSON with nested objects (the
// shape that motivated this helper — see linearreconciler/metareconciler).
func TestExtractFromBody(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("x"))
	bodyTmpl := template.Must(template.New("body").Parse("x"))
	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := &testData{
		PackageName: "pkg",
		Version:     "1.2.3",
		Commit:      "abcdef",
		Nested:      &testNested{Field1: "hello", Field2: 42},
	}
	body, err := cm.templateExecutor.Embed("body text", &embeddedData[testData]{Data: *want})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	got, err := cm.Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.PackageName != want.PackageName || got.Version != want.Version || got.Commit != want.Commit {
		t.Errorf("Extract scalar fields = %+v, want %+v", got, want)
	}
	if got.Nested == nil || got.Nested.Field1 != want.Nested.Field1 || got.Nested.Field2 != want.Nested.Field2 {
		t.Errorf("Extract nested = %+v, want %+v", got.Nested, want.Nested)
	}

	if _, err := cm.Extract("PR body with no embedded data"); err == nil {
		t.Error("Extract on body without data: got nil error, want non-nil")
	}
}

// TestExtractLegacyFormat verifies Extract still recovers data from PR bodies
// created before the embeddedData wrapper, which embed the caller's data bare.
func TestExtractLegacyFormat(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("x"))
	bodyTmpl := template.Must(template.New("body").Parse("x"))
	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := `PR body text

<!--test-bot-pr-data-->
<!--
{
  "PackageName": "pkg",
  "Version": "1.2.3",
  "Commit": "abcdef"
}
-->
<!--/test-bot-pr-data-->`

	got, err := cm.Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.PackageName != "pkg" || got.Version != "1.2.3" || got.Commit != "abcdef" {
		t.Errorf("Extract = %+v, want pkg/1.2.3/abcdef", got)
	}
}

// TestExtractFromBody_RealisticPRBody mirrors the surface-area of a real PR
// body produced by a downstream consumer (markdown link, em-dash, code
// fences) to catch regressions where Extract becomes sensitive to the body
// content surrounding the embedded data block.
func TestExtractFromBody_RealisticPRBody(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}"))
	bodyTmpl := template.Must(template.New("body").Parse(
		`Materializing from [{{.PackageName}}](https://example.com/{{.PackageName}}).

{{.Version}} — see ` + "`{{.Commit}}`" + ` for context.

---
*Generated by test-bot*`))
	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := &testData{
		PackageName: "pkg",
		Version:     "1.2.3",
		Commit:      "abcdef",
		Nested:      &testNested{Field1: "with-dashes-and_underscores", Field2: 99},
	}
	rendered, err := cm.render(bodyTmpl, want)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body, err := cm.templateExecutor.Embed(rendered, &embeddedData[testData]{Data: *want})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	got, err := cm.Extract(body)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.PackageName != want.PackageName || got.Nested == nil || got.Nested.Field2 != want.Nested.Field2 {
		t.Errorf("Extract = %+v, want %+v", got, want)
	}
}

// TestCollectFindings locks in the client-side classification of statusCheckRollup
// contexts that replaced the server-side checkSuites filterBy: a FAILURE conclusion
// becomes a finding, a not-yet-complete status becomes a pending check, and
// success/neutral/cancelled runs plus non-CheckRun contexts are ignored. The
// cancelled case matters — the old query used filterBy:{conclusions:[FAILURE]},
// so only FAILURE (not CANCELLED/TIMED_OUT/etc.) was ever treated as a failure.
func TestCollectFindings(t *testing.T) {
	cr := func(id int64, name, status, conclusion string) gqlStatusCheckRollupContext {
		return gqlStatusCheckRollupContext{
			Typename: "CheckRun",
			CheckRun: gqlCheckRunNode{
				DatabaseId: id,
				Name:       name,
				Status:     status,
				Conclusion: conclusion,
				DetailsUrl: "https://ci/" + name,
			},
		}
	}

	contexts := gqlRollupContextsConnection{
		Nodes: []gqlStatusCheckRollupContext{
			cr(1, "build", "COMPLETED", "FAILURE"),
			cr(2, "unit-tests", "IN_PROGRESS", ""),
			cr(3, "lint", "QUEUED", ""),
			cr(4, "vet", "COMPLETED", "SUCCESS"),
			cr(5, "sbom", "COMPLETED", "NEUTRAL"),
			cr(6, "flaky", "COMPLETED", "CANCELLED"), // not FAILURE -> not a finding
			{Typename: "StatusContext"},              // legacy commit status -> ignored
		},
		// PageInfo.HasNextPage is false, so paginateRollupContexts is never called
		// and the nil gqlClient is safe.
	}

	findings, pending, err := collectFindings(t.Context(), nil, "owner", "repo", "sha", contexts)
	if err != nil {
		t.Fatalf("collectFindings: %v", err)
	}

	if len(findings) != 1 {
		t.Fatalf("findings: got %d, want 1 (%+v)", len(findings), findings)
	}
	if f := findings[0]; f.Name != "build" || f.Identifier != "1" ||
		f.Kind != callbacks.FindingKindCICheck || f.DetailsURL != "https://ci/build" {
		t.Errorf("unexpected finding: %+v", f)
	}

	if want := []string{"unit-tests", "lint"}; !slices.Equal(pending, want) {
		t.Errorf("pendingChecks: got %v, want %v", pending, want)
	}
}

// handlerRoundTripper serves every request from an http.Handler, letting a
// test intercept the GraphQL calls the shurcooL client sends to api.github.com.
type handlerRoundTripper struct {
	handler http.Handler
}

func (rt handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func newTestGraphQLClient(t *testing.T, handler http.Handler) *graphqlclient.GraphQLClient {
	t.Helper()
	gh, err := github.NewClient(github.WithHTTPClient(&http.Client{
		Transport: handlerRoundTripper{handler: handler},
	}))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return graphqlclient.NewGraphQLClient(gh)
}

// TestCollectFindings_PaginatesRollupContexts drives collectFindings through
// two pagination requests (three pages total) and verifies that findings and
// pending checks merge across all pages and that each request carries the
// previous page's end cursor.
func TestCollectFindings_PaginatesRollupContexts(t *testing.T) {
	pages := []string{
		`{"data": {"repository": {"object": {"statusCheckRollup": {"contexts": {
		   "pageInfo": {"hasNextPage": true, "endCursor": "cursor-2"},
		   "nodes": [
		     {"__typename": "CheckRun", "databaseId": 3, "name": "integration",
		      "status": "COMPLETED", "conclusion": "FAILURE", "detailsUrl": "https://ci/integration", "title": "", "summary": "", "text": ""},
		     {"__typename": "CheckRun", "databaseId": 4, "name": "docs",
		      "status": "COMPLETED", "conclusion": "SUCCESS", "detailsUrl": "", "title": "", "summary": "", "text": ""}
		   ]
		 }}}}}}`,
		`{"data": {"repository": {"object": {"statusCheckRollup": {"contexts": {
		   "pageInfo": {"hasNextPage": false, "endCursor": ""},
		   "nodes": [
		     {"__typename": "CheckRun", "databaseId": 5, "name": "e2e",
		      "status": "QUEUED", "conclusion": "", "detailsUrl": "", "title": "", "summary": "", "text": ""}
		   ]
		 }}}}}}`,
	}

	var gotCursors []string
	gqlClient := newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables struct {
				Cursor string `json:"cursor"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		gotCursors = append(gotCursors, body.Variables.Cursor)

		w.Header().Set("Content-Type", "application/json")
		page := min(len(gotCursors), len(pages)) - 1
		if _, err := io.WriteString(w, pages[page]); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))

	initial := gqlRollupContextsConnection{
		Nodes: []gqlStatusCheckRollupContext{
			{Typename: "CheckRun", CheckRun: gqlCheckRunNode{DatabaseId: 1, Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"}},
			{Typename: "CheckRun", CheckRun: gqlCheckRunNode{DatabaseId: 2, Name: "unit-tests", Status: "IN_PROGRESS"}},
		},
	}
	initial.PageInfo.HasNextPage = true
	initial.PageInfo.EndCursor = "cursor-1"

	findings, pending, err := collectFindings(t.Context(), gqlClient, "owner", "repo", "sha", initial)
	if err != nil {
		t.Fatalf("collectFindings: %v", err)
	}

	var findingNames []string
	for _, f := range findings {
		findingNames = append(findingNames, f.Name)
	}
	if want := []string{"build", "integration"}; !slices.Equal(findingNames, want) {
		t.Errorf("findings: got %v, want %v", findingNames, want)
	}
	if want := []string{"unit-tests", "e2e"}; !slices.Equal(pending, want) {
		t.Errorf("pendingChecks: got %v, want %v", pending, want)
	}
	if want := []string{"cursor-1", "cursor-2"}; !slices.Equal(gotCursors, want) {
		t.Errorf("request cursors: got %v, want %v", gotCursors, want)
	}
}

// TestCollectFindings_PaginationErrorPropagates verifies that a failed
// pagination request fails collectFindings instead of silently truncating
// findings, which would let a red or pending PR read as green downstream.
func TestCollectFindings_PaginationErrorPropagates(t *testing.T) {
	gqlClient := newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))

	initial := gqlRollupContextsConnection{
		Nodes: []gqlStatusCheckRollupContext{
			{Typename: "CheckRun", CheckRun: gqlCheckRunNode{DatabaseId: 1, Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"}},
		},
	}
	initial.PageInfo.HasNextPage = true
	initial.PageInfo.EndCursor = "cursor-1"

	findings, pending, err := collectFindings(t.Context(), gqlClient, "owner", "repo", "sha", initial)
	if err == nil {
		t.Fatal("collectFindings: got nil error, want pagination error")
	}
	if want := "paginating status check rollup contexts"; !strings.Contains(err.Error(), want) {
		t.Errorf("error: got %q, want containing %q", err, want)
	}
	if findings != nil || pending != nil {
		t.Errorf("partial results returned alongside error: findings=%v pending=%v", findings, pending)
	}
}
