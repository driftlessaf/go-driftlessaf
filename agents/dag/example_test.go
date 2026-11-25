/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dag_test

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/dag"
)

// Example runs a diamond: review and critique both read draft, and merge reads
// both of them.
func Example() {
	echo := func(name string) dag.StepRunner {
		return func(_ context.Context, in dag.Input) ([]byte, error) {
			parts := []string{name}
			for _, k := range []string{"draft", "review", "critique"} {
				if v, ok := in.Reads[k]; ok {
					parts = append(parts, k+"=("+string(v)+")")
				}
			}
			return []byte(strings.Join(parts, " ")), nil
		}
	}
	g, err := dag.New([]dag.Step{
		{Name: "draft", Run: echo("draft")},
		{Name: "review", Deps: []string{"draft"}, Run: echo("review")},
		{Name: "critique", Deps: []string{"draft"}, Run: echo("critique")},
		{Name: "merge", Deps: []string{"review", "critique"}, Run: echo("merge")},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	res := g.Run(context.Background(), dag.WithMaxConcurrency(2))
	fmt.Println(res.Steps["merge"].Status)
	fmt.Println(string(res.Steps["merge"].Output))
	// Output:
	// succeeded
	// merge review=(review draft=(draft)) critique=(critique draft=(draft))
}
