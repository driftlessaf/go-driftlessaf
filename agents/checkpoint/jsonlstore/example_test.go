/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package jsonlstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/jsonlstore"
)

// ExampleNew demonstrates that a parked envelope survives a process restart:
// a fresh open of the same JSONL log replays it and serves the same state.
func ExampleNew() {
	dir, err := os.MkdirTemp("", "jsonlstore-example")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "checkpoints.jsonl")

	ctx := context.Background()
	first, err := jsonlstore.New(path)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	env := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		Provider:      checkpoint.ProviderAnthropic,
		ReconcilerKey: "org/repo#7",
		RunID:         "run-7",
		ProviderState: json.RawMessage(`{"model":"claude-fable-5"}`),
	}
	if err := first.Save(ctx, env.ReconcilerKey, env); err != nil {
		fmt.Println("error:", err)
		return
	}

	// A second open (a "restarted process") replays the log.
	second, err := jsonlstore.New(path)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	loaded, _, ok, err := second.Load(ctx, env.ReconcilerKey)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("survived reopen:", ok, loaded.RunID)
	// Output: survived reopen: true run-7
}
