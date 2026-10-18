/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package batch_test

import (
	"context"
	"errors"

	"cloud.google.com/go/storage"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"

	"chainguard.dev/driftlessaf/agents/executor/batch"
	"chainguard.dev/driftlessaf/agents/metrics"
)

// Example submits two Claude requests under a stable id, then, on a later
// pass, checks the job and reads its results. It needs Vertex AI and a
// bucket the Vertex AI service agent can read and write, so it is not run.
func Example() {
	ctx := context.Background()
	gc, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  "my-project",
		Location: "us",
	})
	if err != nil {
		panic(err)
	}
	sc, err := storage.NewClient(ctx)
	if err != nil {
		panic(err)
	}
	defer sc.Close()

	client, err := batch.NewClient(gc, sc.Bucket("my-project-batch"), "jobs",
		batch.WithMetrics(metrics.NewGenAI("chainguard.ai.agents")))
	if err != nil {
		panic(err)
	}

	params := func(prompt string) anthropic.MessageNewParams {
		return anthropic.MessageNewParams{
			MaxTokens: 4096,
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
		}
	}
	// The id is the caller's stable identity for this set of rows: running
	// this again after a crash returns the same job.
	job, err := client.SubmitClaude(ctx, "review-0a1b2c", "claude-opus-5-5@default", []batch.ClaudeRequest{
		{Key: "skill-a", Params: params("Review skill A.")},
		{Key: "skill-b", Params: params("Review skill B.")},
	})
	if err != nil {
		panic(err)
	}

	// Later: check the job, and collect once it is terminal.
	st, err := client.Status(ctx, job)
	if err != nil {
		panic(err)
	}
	if !st.Terminal {
		return
	}
	for res, err := range client.Results(ctx, job) {
		if err != nil {
			panic(err)
		}
		switch {
		case res.Err == nil:
			clog.InfoContextf(ctx, "%s: %d output tokens", res.Key, res.Usage.Out)
		case errors.Is(res.Err, batch.ErrRefused):
			clog.InfoContextf(ctx, "%s: refused (%s)", res.Key, res.Refusal.Category)
		case errors.Is(res.Err, batch.ErrTruncated), errors.Is(res.Err, batch.ErrMissing), batch.IsRetryable(res.Err):
			clog.InfoContextf(ctx, "%s: resubmit under a new id", res.Key)
		default:
			clog.InfoContextf(ctx, "%s: %v", res.Key, res.Err)
		}
	}
}

// ExampleClient_Find looks up a job by its submission id without
// submitting.
func ExampleClient_Find() {
	ctx := context.Background()
	var client *batch.Client // from batch.NewClient
	job, ok, err := client.Find(ctx, "review-0a1b2c")
	if err != nil {
		panic(err)
	}
	if ok {
		clog.InfoContextf(ctx, "job %s has %d requests", job.Name, job.Keys)
	}
}
