/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package runbudget_test

import (
	"context"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/runbudget"
)

func ExampleStart() {
	b := runbudget.Budget{Total: 55 * time.Minute, Reserve: 8 * time.Minute}
	start := time.Now()

	// The request context, as the platform handed it to the reconcile.
	request, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	ctx := runbudget.Start(request, b, start)

	// The agent phase inherits request cancellation and carries the agent deadline.
	agent, cancelAgent := runbudget.AgentContext(ctx)
	defer cancelAgent()
	deadline, _ := agent.Deadline()
	fmt.Println("agent phase length:", deadline.Sub(start))

	// The publication phase is detached: a request that ends now cannot stop it.
	publication, cancelPublication := runbudget.PublicationContext(ctx)
	defer cancelPublication()
	cancelRequest()
	fmt.Println("publication still live:", publication.Err() == nil)
	// Output:
	// agent phase length: 47m0s
	// publication still live: true
}

func ExampleClassify() {
	request := context.Background()
	agent, cancel := context.WithDeadline(request, time.Now().Add(-time.Second))
	defer cancel()

	// The agent returned an error after its context reached the budget's deadline
	// while the request itself was still live.
	fmt.Println(runbudget.Classify(request, agent, agent.Err()))

	// A result that arrived is accepted whatever the clocks say.
	fmt.Println(runbudget.Classify(request, agent, nil))
	// Output:
	// deadline
	// finished
}

func ExampleStarted() {
	ctx := runbudget.Start(context.Background(), runbudget.Budget{Total: time.Hour, Reserve: 8 * time.Minute}, time.Now())
	b, _, ok := runbudget.Started(ctx)
	fmt.Println(ok, b.Total, b.Reserve)
	// Output:
	// true 1h0m0s 8m0s
}
