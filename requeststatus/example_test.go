/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/requeststatus"
)

// Example builds a Validator with the Running and Waiting reasons a
// lifecycle authority owns, then validates a Running and a Complete update.
func Example() {
	v, err := requeststatus.NewValidator(
		requeststatus.WithReasons(requeststatus.PhaseRunning, "initial", "merge-conflict", "ci-fix"),
		requeststatus.WithReasons(requeststatus.PhaseWaiting, "ci", "review"),
		requeststatus.WithLinkHosts("github.com"),
	)
	if err != nil {
		fmt.Println("building validator:", err)
		return
	}

	running := requeststatus.Update{
		Phase:    requeststatus.PhaseRunning,
		Reason:   "ci-fix",
		Activity: requeststatus.ActivityEnrich,
		Attempt:  2,
		Change: requeststatus.ChangeLink{
			Label: "PR #42",
			URL:   "https://github.com/example/repo/pull/42",
		},
	}
	fmt.Println("running valid:", v.Validate(running) == nil)

	complete := requeststatus.Update{
		Outcome: requeststatus.OutcomeComplete,
		Reason:  "merged",
	}
	fmt.Println("complete valid:", v.Validate(complete) == nil)

	// A request surface and a change surface are bound by Role, not by
	// host: the same Update can be published to either.
	roles := []requeststatus.Role{requeststatus.RoleRequest, requeststatus.RoleChange}
	fmt.Println("roles:", roles)

	// Output:
	// running valid: true
	// complete valid: true
	// roles: [request change]
}

// ExampleValidateSupersededKeys shows that a legacy key for the same bot is
// safe to delete by prefix, while an empty key, another bot's key, or a
// prefix of the active key are all rejected because a Surface would delete
// too much.
func ExampleValidateSupersededKeys() {
	const botKey = "manifest-gen"
	active := requeststatus.EntryKey("manifest-gen:request-status")

	err := requeststatus.ValidateSupersededKeys(botKey, active, "manifest-gen:start", "manifest-gen:failure")
	fmt.Println("legacy keys ok:", err == nil)

	err = requeststatus.ValidateSupersededKeys(botKey, active, "manifest-gen:")
	fmt.Println("bot-key-only key ok:", err == nil)

	// Output:
	// legacy keys ok: true
	// bot-key-only key ok: false
}

// memorySurface is a minimal, non-host-backed Surface used only to
// demonstrate the interface contract: one entry per EntryKey, and no write
// when the Document is unchanged.
type memorySurface struct {
	published map[requeststatus.EntryKey]requeststatus.Document
	writes    int
}

func (s *memorySurface) Publish(_ context.Context, key requeststatus.EntryKey, doc requeststatus.Document) error {
	if existing, ok := s.published[key]; ok && existing == doc {
		return nil
	}
	s.published[key] = doc
	s.writes++
	return nil
}

func (s *memorySurface) RemoveSuperseded(_ context.Context, _ ...requeststatus.EntryKey) error {
	return nil
}

// ExampleSurface publishes the same Document twice for the same EntryKey
// and shows that the second call performs no write, per the Surface
// contract.
func ExampleSurface() {
	var surface requeststatus.Surface = &memorySurface{published: map[requeststatus.EntryKey]requeststatus.Document{}}

	key := requeststatus.EntryKey("manifest-gen:request-status")
	doc := requeststatus.Document{}

	ctx := context.Background()
	_ = surface.Publish(ctx, key, doc)
	_ = surface.Publish(ctx, key, doc)

	fmt.Println("writes:", surface.(*memorySurface).writes)

	// Output:
	// writes: 1
}
