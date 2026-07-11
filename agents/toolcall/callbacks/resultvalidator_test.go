/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// testResult is the response type validated across these tests.
type testResult struct {
	Answer string
}

// acceptAll is a validator that accepts every result.
func acceptAll(context.Context, testResult, string) ([]Finding, error) {
	return nil, nil
}

func TestValidateResultNoValidators(t *testing.T) {
	t.Parallel()

	findings, err := ValidateResult(t.Context(), nil, testResult{Answer: "a"}, "reasoning")
	if err != nil {
		t.Fatalf("ValidateResult: got = %v, want = nil", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings: got = %d, want = 0", len(findings))
	}
}

func TestValidateResultAllAccept(t *testing.T) {
	t.Parallel()

	findings, err := ValidateResult(t.Context(), []ResultValidator[testResult]{
		acceptAll,
		acceptAll,
	}, testResult{Answer: "a"}, "reasoning")
	if err != nil {
		t.Fatalf("ValidateResult: got = %v, want = nil", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings: got = %d, want = 0", len(findings))
	}
}

func TestValidateResultFlattensInRegistrationOrder(t *testing.T) {
	t.Parallel()

	// The first validator finishes last, so ordered output proves the flatten
	// follows registration order rather than completion order.
	findings, err := ValidateResult(t.Context(), []ResultValidator[testResult]{
		func(context.Context, testResult, string) ([]Finding, error) {
			time.Sleep(50 * time.Millisecond)
			return []Finding{{
				Kind:       FindingKindReview,
				Identifier: "first-a",
			}, {
				Kind:       FindingKindReview,
				Identifier: "first-b",
			}}, nil
		},
		func(context.Context, testResult, string) ([]Finding, error) {
			return []Finding{{
				Kind:       FindingKindReview,
				Identifier: "second-a",
			}}, nil
		},
	}, testResult{Answer: "a"}, "reasoning")
	if err != nil {
		t.Fatalf("ValidateResult: got = %v, want = nil", err)
	}

	got := make([]string, 0, len(findings))
	for _, f := range findings {
		got = append(got, f.Identifier)
	}
	want := []string{"first-a", "first-b", "second-a"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("finding order (-want, +got):\n%s", diff)
	}
}

func TestValidateResultErrorFailsLoud(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("judge unavailable")
	findings, err := ValidateResult(t.Context(), []ResultValidator[testResult]{
		func(context.Context, testResult, string) ([]Finding, error) {
			return []Finding{{Kind: FindingKindReview, Identifier: "dropped"}}, nil
		},
		func(context.Context, testResult, string) ([]Finding, error) {
			return nil, wantErr
		},
	}, testResult{Answer: "a"}, "reasoning")
	if !errors.Is(err, wantErr) {
		t.Fatalf("ValidateResult error: got = %v, want = %v", err, wantErr)
	}
	if findings != nil {
		t.Errorf("findings on error: got = %v, want = nil", findings)
	}
}

func TestValidateResultRunsValidatorsConcurrently(t *testing.T) {
	t.Parallel()

	// Each validator holds for 50ms while tracking the number in flight; a
	// maximum above 1 proves concurrent dispatch.
	var inFlight, maxInFlight atomic.Int32
	hold := func(context.Context, testResult, string) ([]Finding, error) {
		cur := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return nil, nil
	}

	if _, err := ValidateResult(t.Context(), []ResultValidator[testResult]{hold, hold, hold}, testResult{}, ""); err != nil {
		t.Fatalf("ValidateResult: got = %v, want = nil", err)
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Errorf("max concurrent validators: got = %d, want >= 2 (validators did not run concurrently)", got)
	}
}

func TestValidateResultPassesResponseAndReasoning(t *testing.T) {
	t.Parallel()

	wantAnswer := fmt.Sprintf("answer-%d", time.Now().UnixNano())
	wantReasoning := fmt.Sprintf("reasoning-%d", time.Now().UnixNano())

	var gotAnswer, gotReasoning string
	if _, err := ValidateResult(t.Context(), []ResultValidator[testResult]{
		func(_ context.Context, r testResult, reasoning string) ([]Finding, error) {
			gotAnswer, gotReasoning = r.Answer, reasoning
			return nil, nil
		},
	}, testResult{Answer: wantAnswer}, wantReasoning); err != nil {
		t.Fatalf("ValidateResult: got = %v, want = nil", err)
	}
	if gotAnswer != wantAnswer {
		t.Errorf("response answer: got = %q, want = %q", gotAnswer, wantAnswer)
	}
	if gotReasoning != wantReasoning {
		t.Errorf("reasoning: got = %q, want = %q", gotReasoning, wantReasoning)
	}
}

func TestRejectionResult(t *testing.T) {
	t.Parallel()

	findings := []Finding{{
		Kind:       FindingKindReview,
		Identifier: "empty-answer",
		Details:    "the answer field is empty",
	}, {
		Kind:       FindingKindReview,
		Identifier: "low-confidence",
		Details:    "confidence is below the acceptance threshold",
	}}

	result := RejectionResult("submit_result", findings)

	errText, ok := result["error"].(string)
	if !ok {
		t.Fatalf("error field: got = %T, want = string", result["error"])
	}
	if !strings.Contains(errText, "2 finding(s)") {
		t.Errorf("error text: got = %q, want mention of finding count", errText)
	}
	if !strings.Contains(errText, "submit_result") {
		t.Errorf("error text: got = %q, want mention of submit tool name", errText)
	}

	gotFindings, ok := result["findings"].([]Finding)
	if !ok {
		t.Fatalf("findings field: got = %T, want = []Finding", result["findings"])
	}
	if diff := cmp.Diff(findings, gotFindings); diff != "" {
		t.Errorf("findings (-want, +got):\n%s", diff)
	}
}
