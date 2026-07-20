/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/checkpoint/memstore"
)

// ExampleNewAskHumanSuspension demonstrates the suspend half of the lifecycle:
// an executor assembles a Suspension when the model calls its held-out
// ask-human tool, returns it as an ordinary error, and the reconciler at the
// top extracts it with AsSuspension.
func ExampleNewAskHumanSuspension() {
	s := checkpoint.NewAskHumanSuspension(
		checkpoint.ProviderAnthropic, "claude-fable-5", "sha256:cfg",
		3, 12,
		checkpoint.PendingToolCall{
			ID:        "toolu_01ABC",
			Name:      "ask_human",
			InputJSON: json.RawMessage(`{"question":"Should I force-push?"}`),
		},
		json.RawMessage(`{"model":"claude-fable-5","max_tokens":1024}`),
		json.RawMessage(`{"turn":3}`),
		"trace-abc",
	)

	// The envelope is validated before it is parked, so an unreplayable
	// checkpoint fails at suspend time rather than at resume.
	fmt.Println("parkable:", s.Validate())

	// The Suspension travels out of the executor as an ordinary error, through
	// any number of fmt.Errorf wrappers...
	err := fmt.Errorf("executing turn 3: %w", error(s))

	// ...and the caller at the top extracts it.
	got, ok := checkpoint.AsSuspension(err)
	fmt.Println("suspended:", ok)
	fmt.Println("question:", got.Question)
	fmt.Println("remaining turns:", got.RemainingTurns)
	// Output:
	// parkable: <nil>
	// suspended: true
	// question: Should I force-push?
	// remaining turns: 8
}

// ExampleFrameAnswer demonstrates framing a raw human answer for injection as
// a tool result: delimited, and never empty on the wire.
func ExampleFrameAnswer() {
	fmt.Println(checkpoint.FrameAnswer("Yes, go ahead.", 0))
	fmt.Println(checkpoint.FrameAnswer("   ", 0))
	// Output:
	// <<<BEGIN HUMAN ANSWER>>>
	// Yes, go ahead.
	// <<<END HUMAN ANSWER>>>
	// <<<BEGIN HUMAN ANSWER>>>
	// (the human did not provide an answer)
	// <<<END HUMAN ANSWER>>>
}

// ExampleFramedAnswers demonstrates answering every pending tool call from an
// envelope: a call with no supplied answer gets the explicit placeholder so no
// provider request ever carries an unanswered tool call.
func ExampleFramedAnswers() {
	pending := []checkpoint.PendingToolCall{
		{ID: "toolu_01", Name: "ask_human"},
		{ID: "toolu_02", Name: "sibling_tool"},
	}
	framed, err := checkpoint.FramedAnswers(pending, map[string]string{
		"toolu_01": "Ship it.",
	}, 0)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, fa := range framed {
		fmt.Printf("%s (%s):\n%s\n", fa.ID, fa.Name, fa.Text)
	}
	// Output:
	// toolu_01 (ask_human):
	// <<<BEGIN HUMAN ANSWER>>>
	// Ship it.
	// <<<END HUMAN ANSWER>>>
	// toolu_02 (sibling_tool):
	// <<<BEGIN HUMAN ANSWER>>>
	// (the human did not provide an answer)
	// <<<END HUMAN ANSWER>>>
}

// ExampleValidateForResume demonstrates the fail-closed gate on the wake side:
// a parked envelope only resumes against the exact executor configuration that
// produced it (any drift surfaces as ErrConfigDrift) and only before its
// park-time Deadline.
func ExampleValidateForResume() {
	env := checkpoint.Envelope{
		Version:        checkpoint.EnvelopeVersion,
		Provider:       checkpoint.ProviderAnthropic,
		Model:          "claude-fable-5",
		ConfigDigest:   "sha256:cfg",
		RemainingTurns: 4,
		Deadline:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}

	// The live executor matches the parked envelope: resume may proceed.
	fmt.Println("match:", checkpoint.ValidateForResume(env,
		checkpoint.ProviderAnthropic, "claude-fable-5", "sha256:cfg",
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)))

	// The model drifted under the pause: rebuild from scratch instead.
	err := checkpoint.ValidateForResume(env,
		checkpoint.ProviderAnthropic, "claude-fable-6", "sha256:cfg",
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	fmt.Println("drift:", errors.Is(err, checkpoint.ErrConfigDrift))

	// The wake arrived after the envelope's deadline: fail closed.
	err = checkpoint.ValidateForResume(env,
		checkpoint.ProviderAnthropic, "claude-fable-5", "sha256:cfg",
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	fmt.Println("expired:", err != nil)
	// Output:
	// match: <nil>
	// drift: true
	// expired: true
}

// ExampleDigestJSON demonstrates stamping and re-deriving a config digest: the
// digest is stable for identical configuration and changes when any
// resume-relevant field drifts.
func ExampleDigestJSON() {
	type config struct {
		Model    string
		MaxTurns int
	}

	parked, err := checkpoint.DigestJSON(config{Model: "claude-fable-5", MaxTurns: 12})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	live, err := checkpoint.DigestJSON(config{Model: "claude-fable-5", MaxTurns: 12})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	drifted, err := checkpoint.DigestJSON(config{Model: "claude-fable-5", MaxTurns: 6})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("stable:", parked == live)
	fmt.Println("drift detected:", parked != drifted)
	// Output:
	// stable: true
	// drift detected: true
}

// Example_storeRoundTrip demonstrates the park/claim half of the lifecycle
// against the in-memory Store: Save parks the envelope, Load returns it with a
// CAS token, and Delete with that token claims it exactly once.
func Example_storeRoundTrip() {
	ctx := context.Background()
	store := memstore.New()

	env := &checkpoint.Envelope{
		Version:       checkpoint.EnvelopeVersion,
		Provider:      checkpoint.ProviderAnthropic,
		ReconcilerKey: "org/repo#42",
		RunID:         "run-1",
		ProviderState: json.RawMessage(`{"model":"claude-fable-5"}`),
	}
	if err := store.Save(ctx, env.ReconcilerKey, env); err != nil {
		fmt.Println("error:", err)
		return
	}

	loaded, tok, ok, err := store.Load(ctx, env.ReconcilerKey)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("parked:", ok, loaded.RunID)

	// The first Delete with the token wins the claim; a second attempt with
	// the now-stale token loses the race.
	fmt.Println("claimed:", store.Delete(ctx, env.ReconcilerKey, tok))
	fmt.Println("second claim lost:",
		errors.Is(store.Delete(ctx, env.ReconcilerKey, tok), checkpoint.ErrTokenMismatch))
	// Output:
	// parked: true run-1
	// claimed: <nil>
	// second claim lost: true
}
