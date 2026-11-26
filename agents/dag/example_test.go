/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dag_test

import (
	"bytes"
	"context"
	"errors"
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

// ExampleIterate keeps a capped loop's best effort: under Graph.Run a step that
// returns ErrMaxIterations fails and its output is dropped.
func ExampleIterate() {
	refine := dag.Iterate(2, func(_ context.Context, _ dag.Input, prev []byte) ([]byte, bool, error) {
		return append(bytes.Clone(prev), '+'), false, nil
	})
	bestEffort := func(ctx context.Context, in dag.Input) ([]byte, error) {
		out, err := refine(ctx, in)
		if errors.Is(err, dag.ErrMaxIterations) {
			return out, nil
		}
		return out, err
	}
	g, err := dag.New([]dag.Step{{Name: "draft", Run: bestEffort}})
	if err != nil {
		fmt.Println(err)
		return
	}
	res := g.Run(context.Background())
	fmt.Println(res.Steps["draft"].Status, string(res.Steps["draft"].Output))
	// Output: succeeded ++
}
