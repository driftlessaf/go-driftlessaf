/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package pathpatterns_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/pathpatterns"
)

func ExampleNew() {
	m, err := pathpatterns.New(
		[]string{`(images/[^/]+)/.*`},
		[]string{`.*/testdata/.*`},
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	for _, p := range []string{
		"images/nginx/config/main.tf",
		"images/nginx/testdata/fixture.yaml",
		"README.md",
	} {
		if key, ok := m.Match(p); ok {
			fmt.Printf("matched %q -> key %q\n", p, key)
		} else {
			fmt.Printf("no match for %q\n", p)
		}
	}
	// Output:
	// matched "images/nginx/config/main.tf" -> key "images/nginx"
	// no match for "images/nginx/testdata/fixture.yaml"
	// no match for "README.md"
}

func ExampleNew_invalid() {
	_, err := pathpatterns.New([]string{`images/.+`}, nil)
	fmt.Println(err)
	// Output:
	// regex "^images/.+$" must have exactly one capture group, got 0
}

func ExampleMatcher_Match() {
	m, err := pathpatterns.New([]string{`(packages/.+\.yaml)`}, nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	for _, p := range []string{
		"packages/foo.yaml",
		"packages/nested/bar.yaml",
		"other/file.go",
	} {
		key, ok := m.Match(p)
		fmt.Printf("%q -> %q, %t\n", p, key, ok)
	}
	// Output:
	// "packages/foo.yaml" -> "packages/foo.yaml", true
	// "packages/nested/bar.yaml" -> "packages/nested/bar.yaml", true
	// "other/file.go" -> "", false
}
