/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager_test

import (
	"context"
	"errors"
	"fmt"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"github.com/google/go-github/v88/github"
)

type UpdateData struct {
	PackageName string
	Version     string
	Commit      string
}

func Example() {
	// Parse templates once at initialization
	titleTmpl := template.Must(template.New("title").Parse(`{{.PackageName}}/{{.Version}} package update`))
	bodyTmpl := template.Must(template.New("body").Parse(`Update {{.PackageName}} to {{.Version}}

{{if .Commit}}**Commit**: {{.Commit}}{{end}}`))

	// Create manager once per identity
	cm, err := changemanager.New[UpdateData]("update-bot", titleTmpl, bodyTmpl)
	if err != nil {
		// handle error
		return
	}

	// In your reconciler, create a session per resource
	ctx := context.Background()
	var ghClient *github.Client // your GitHub client
	var res *githubreconciler.Resource

	session, err := cm.NewSession(ctx, ghClient, res)
	if err != nil {
		// handle error
		return
	}

	// Check if the PR should be skipped
	if session.ShouldSkip() {
		// skip this resource
		return
	}

	// Upsert a PR with data
	_, err = session.Upsert(ctx, &UpdateData{
		PackageName: "foo",
		Version:     "1.2.3",
		Commit:      "abc123",
	}, false, []string{"automated pr"}, func(_ context.Context, _ string) error {
		// Make code changes on the branch.
		// Return changemanager.ErrNoChanges if no diff was produced.
		return nil
	})
	if errors.Is(err, changemanager.ErrNoChanges) {
		// No diff was produced — close any existing PR, log, or ignore.
		_ = session.CloseAnyOutstanding(ctx, "Closing PR because all changes have been resolved.")
		return
	}
	if err != nil {
		// handle error
		return
	}
}

// ExampleSession_AppendReasoning demonstrates accumulating a per-commit
// reasoning log across iterations. Inside the Upsert makeChanges callback,
// once a commit is certain, record the run's reasoning against the commit's
// headline; the log persists in the PR body, so ReasoningLog returns entries
// from prior iterations too, ready to render one reasoning block per commit.
func ExampleSession_AppendReasoning() {
	var session changemanager.Session[UpdateData]

	session.AppendReasoning("fix(foo): correct frobnication", "- adjusted the frobnicator")
	session.AppendReasoning("ci(e2e): retry flaky step", "- reran the check with backoff")
	session.AppendReasoning("chore: run without reasoning", "") // empty summary: no entry

	for _, e := range session.ReasoningLog() {
		fmt.Printf("%s -> %s\n", e.CommitHeadline, e.Summary)
	}

	// Output:
	// fix(foo): correct frobnication -> - adjusted the frobnicator
	// ci(e2e): retry flaky step -> - reran the check with backoff
}
