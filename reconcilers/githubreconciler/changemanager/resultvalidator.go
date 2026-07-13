/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"reflect"
	"strings"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// resultContract is the slice of an agent result the change manager consumes:
// a commit message to drive the PR (the reconcilers' Result interface) and a
// no-change explanation to drive the give-up comment (Explainer).
type resultContract interface {
	GetCommitMessage() string
	GetNoChangeExplanation() string
}

// ResultValidator returns a validator gating a reconciler agent's terminal
// submit tool on the change manager's contract: a result must carry a commit
// message (the agent made a change) or a no-change explanation (it
// deliberately did not), never neither. Submissions with neither are
// rejected back to the model to state one or the other, instead of the run
// ending with an outcome the reconciler can neither commit nor explain on
// the PR. Register it in the agent's metaagent.Config.ResultValidators.
func ResultValidator[R resultContract]() callbacks.ResultValidator[R] {
	return func(_ context.Context, r R, _ string) ([]callbacks.Finding, error) {
		// A typed-nil pointer still satisfies the constraint, and the
		// accessors have pointer receivers, so guard before calling them.
		if rv := reflect.ValueOf(r); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return []callbacks.Finding{{
				Kind:       callbacks.FindingKindReview,
				Identifier: "missing-result",
				Details:    "the result payload is null; submit the complete result",
			}}, nil
		}
		if strings.TrimSpace(r.GetCommitMessage()) == "" && strings.TrimSpace(r.GetNoChangeExplanation()) == "" {
			return []callbacks.Finding{{
				Kind:       callbacks.FindingKindReview,
				Identifier: "no-commit-message-or-reason",
				Details:    "commit_message and no_changes_reason are both empty; set commit_message if you made a change, or explain in no_changes_reason why there was no in-scope change to make",
			}}, nil
		}
		return nil, nil
	}
}
