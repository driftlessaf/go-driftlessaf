/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager_test

import (
	"context"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"github.com/google/go-github/v75/github"
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
		// Make code changes on the branch
		// e.g., update package YAML, commit changes, push to remote
		return nil
	})
	if err != nil {
		// handle error
		return
	}
}
