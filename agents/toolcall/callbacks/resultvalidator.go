/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import (
	"context"
	"fmt"
	"slices"

	"golang.org/x/sync/errgroup"
)

// ResultValidator inspects a result an agent submitted via its terminal
// submit tool and returns findings describing why the result is not
// acceptable. An empty return means the validator accepts the result.
//
// Validators are registered on an executor (one or more, via its
// WithResultValidator option) and run when the model calls the submit tool
// with a payload that parses into the Response type. When any validator
// returns findings, the result is rejected: it does not become the run's
// final result, and the findings are returned to the model as the submit
// tool's result so it can address them and submit again. When every
// validator returns no findings, the result commits and the run ends.
//
// The reasoning argument is the model's own justification for why it
// believes the result is complete and accurate (the submit tool's universal
// reasoning parameter) — useful input for validators that judge the result
// rather than check invariants.
//
// A non-nil error reports that the validator itself failed (for example an
// upstream API error), not that the result is unacceptable; it aborts the
// run. Validators must be safe for concurrent use: they run in parallel with
// each other. The executors begin evaluating a submission only after the
// turn's other tool handlers have completed, so validators that read state
// those handlers produce (worktrees, files) observe the finished state
// rather than racing them.
type ResultValidator[Response any] func(ctx context.Context, response Response, reasoning string) ([]Finding, error)

// ValidateResult runs the validators concurrently against a submitted result
// and returns their findings concatenated in validator registration order. An
// empty return means every validator accepted the result. The first validator
// error cancels the remaining validators and is returned; findings from
// validators that succeeded are discarded in that case.
func ValidateResult[Response any](ctx context.Context, validators []ResultValidator[Response], response Response, reasoning string) ([]Finding, error) {
	if len(validators) == 0 {
		return nil, nil
	}

	perValidator := make([][]Finding, len(validators))
	g, gctx := errgroup.WithContext(ctx)
	for i, validate := range validators {
		g.Go(func() error {
			findings, err := validate(gctx, response, reasoning)
			if err != nil {
				return err
			}
			perValidator[i] = findings
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return slices.Concat(perValidator...), nil
}

// RejectionResult builds the tool result a submit tool returns to the model
// when validators rejected its submission. The findings ride alongside the
// error text so the model sees exactly what to address before calling the
// submit tool again.
func RejectionResult(submitToolName string, findings []Finding) map[string]any {
	return map[string]any{
		"error": fmt.Sprintf(
			"Result rejected: validation raised %d finding(s). Address each finding and call %s again with your corrected, complete result.",
			len(findings), submitToolName),
		"findings": findings,
	}
}
