/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package issuemanager_test

import (
	"context"
	"slices"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/issuemanager"
	"github.com/google/go-github/v88/github"
)

type ExampleData struct {
	Foo string
	Bar string
	Baz string
}

// Equal implements the Comparable interface for ExampleData.
// It compares Foo and Bar to determine if two instances represent the same issue.
func (e ExampleData) Equal(other ExampleData) bool {
	return e.Foo == other.Foo && e.Bar == other.Bar
}

func Example() {
	// Parse templates once at initialization
	titleTmpl := template.Must(template.New("title").Parse(`Issue {{.Foo}}: {{.Baz}}`))
	bodyTmpl := template.Must(template.New("body").Parse(`This is issue **{{.Foo}}** for {{.Bar}}

**Status**: {{.Baz}}

Additional details here.`))

	// Optional: Define label templates to generate dynamic labels from issue data
	labelTmpl1 := template.Must(template.New("label1").Parse(`status:{{.Baz}}`))
	labelTmpl2 := template.Must(template.New("label2").Parse(`category:{{.Bar}}`))

	// Create manager once per identity with label templates
	im, err := issuemanager.New[ExampleData]("example-manager", titleTmpl, bodyTmpl,
		issuemanager.WithLabelTemplates[ExampleData](labelTmpl1, labelTmpl2),
		issuemanager.WithMaxDesiredIssuesPerPath[ExampleData](10),
	)
	if err != nil {
		// handle error
		return
	}

	// In your reconciler, create a session per resource
	ctx := context.Background()
	var ghClient *github.Client // your GitHub client
	var res *githubreconciler.Resource

	session, err := im.NewSession(ctx, ghClient, res)
	if err != nil {
		// handle error
		return
	}

	// Existing returns the decoded data of the issues already open for this
	// path — the same set Reconcile matches against. Seed the desired-state
	// computation with it so a re-derivation re-confirms tracked items
	// instead of failing to re-discover them and closing their issues.
	desired := make([]*ExampleData, 0, session.MaxDesired())
	for _, prior := range session.Existing() {
		desired = append(desired, &prior)
	}

	// Add newly discovered issues, skipping any already tracked — Reconcile
	// rejects duplicate desired entries.
	// Note: Issues with skip labels (skip:example-manager) will be automatically preserved
	for _, found := range []ExampleData{{
		Foo: "foo",
		Bar: "bar",
		Baz: "baz",
	}, {
		Foo: "bar",
		Bar: "baz",
		Baz: "foo",
	}} {
		if slices.ContainsFunc(desired, func(d *ExampleData) bool { return d.Equal(found) }) {
			continue
		}
		desired = append(desired, &found)
	}

	// Truncate to the session's cap instead of tripping Reconcile's
	// oversized-desired error.
	if len(desired) > session.MaxDesired() {
		desired = desired[:session.MaxDesired()]
	}

	// Reconcile performs a complete reconciliation: create, update, and close operations
	// Matching is done using the Equal method
	_, err = session.Reconcile(ctx, desired, []string{"example", "automated"}, "Issue no longer relevant")
	if err != nil {
		// handle error
		return
	}
}
