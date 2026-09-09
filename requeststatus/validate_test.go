/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus_test

import (
	"testing"

	"chainguard.dev/driftlessaf/requeststatus"
)

func mustValidator(t *testing.T, opts ...requeststatus.Option) *requeststatus.Validator {
	t.Helper()
	v, err := requeststatus.NewValidator(opts...)
	if err != nil {
		t.Fatalf("NewValidator() error = %v", err)
	}
	return v
}

func TestNewValidator(t *testing.T) {
	tests := []struct {
		name    string
		opts    []requeststatus.Option
		wantErr bool
	}{{
		name: "no options",
	}, {
		name: "reasons for running and waiting",
		opts: []requeststatus.Option{
			requeststatus.WithReasons(requeststatus.PhaseRunning, "initial"),
			requeststatus.WithReasons(requeststatus.PhaseWaiting, "ci"),
		},
	}, {
		name:    "reasons for needs_you rejected",
		opts:    []requeststatus.Option{requeststatus.WithReasons(requeststatus.PhaseNeedsYou, "turn-limit")},
		wantErr: true,
	}, {
		name:    "reasons for failed rejected",
		opts:    []requeststatus.Option{requeststatus.WithReasons(requeststatus.PhaseFailed, "no-component")},
		wantErr: true,
	}, {
		name:    "reasons for outcome-shaped phase rejected",
		opts:    []requeststatus.Option{requeststatus.WithReasons(requeststatus.Phase("complete"), "merged")},
		wantErr: true,
	}, {
		name: "link hosts",
		opts: []requeststatus.Option{requeststatus.WithLinkHosts("github.com")},
	}, {
		name:    "empty link host rejected",
		opts:    []requeststatus.Option{requeststatus.WithLinkHosts("github.com", "")},
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := requeststatus.NewValidator(tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewValidator() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatorValidate(t *testing.T) {
	v := mustValidator(t,
		requeststatus.WithReasons(requeststatus.PhaseRunning, "initial", "merge-conflict", "ci-fix"),
		requeststatus.WithReasons(requeststatus.PhaseWaiting, "ci", "review"),
		requeststatus.WithLinkHosts("github.com"),
	)

	tests := []struct {
		name    string
		update  requeststatus.Update
		wantErr bool
	}{{
		name:   "running with declared reason",
		update: requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "initial"},
	}, {
		name:   "running with no reason",
		update: requeststatus.Update{Phase: requeststatus.PhaseRunning},
	}, {
		name:   "running with activity and attempt",
		update: requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "ci-fix", Activity: requeststatus.ActivityEnrich, Attempt: 1},
	}, {
		name:   "waiting with declared reason",
		update: requeststatus.Update{Phase: requeststatus.PhaseWaiting, Reason: "ci"},
	}, {
		name:   "needs_you with reason",
		update: requeststatus.Update{Phase: requeststatus.PhaseNeedsYou, Reason: "turn-limit"},
	}, {
		name:   "failed with reason",
		update: requeststatus.Update{Phase: requeststatus.PhaseFailed, Reason: "model-unreachable"},
	}, {
		name:   "no_action_needed with fixed reason",
		update: requeststatus.Update{Outcome: requeststatus.OutcomeNoActionNeeded, Reason: "already-satisfied"},
	}, {
		name:   "complete with fixed reason",
		update: requeststatus.Update{Outcome: requeststatus.OutcomeComplete, Reason: "merged"},
	}, {
		name:   "canceled with request-closed",
		update: requeststatus.Update{Outcome: requeststatus.OutcomeCanceled, Reason: "request-closed"},
	}, {
		name:   "canceled with required-label-removed",
		update: requeststatus.Update{Outcome: requeststatus.OutcomeCanceled, Reason: "required-label-removed"},
	}, {
		name: "with valid change link",
		update: requeststatus.Update{
			Phase:  requeststatus.PhaseWaiting,
			Reason: "review",
			Change: requeststatus.ChangeLink{Label: "PR #1", URL: "https://github.com/example/repo/pull/1"},
		},
	}, {
		name:    "neither phase nor outcome",
		update:  requeststatus.Update{},
		wantErr: true,
	}, {
		name:    "both phase and outcome",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Outcome: requeststatus.OutcomeComplete, Reason: "merged"},
		wantErr: true,
	}, {
		name:    "unknown phase",
		update:  requeststatus.Update{Phase: requeststatus.Phase("bogus")},
		wantErr: true,
	}, {
		name:    "unknown outcome",
		update:  requeststatus.Update{Outcome: requeststatus.Outcome("bogus")},
		wantErr: true,
	}, {
		name:    "unknown activity",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Activity: requeststatus.Activity("bogus"), Attempt: 1},
		wantErr: true,
	}, {
		name:    "activity on waiting",
		update:  requeststatus.Update{Phase: requeststatus.PhaseWaiting, Reason: "ci", Activity: requeststatus.ActivityEnrich, Attempt: 1},
		wantErr: true,
	}, {
		name:    "activity on needs_you",
		update:  requeststatus.Update{Phase: requeststatus.PhaseNeedsYou, Reason: "turn-limit", Activity: requeststatus.ActivityEnrich, Attempt: 1},
		wantErr: true,
	}, {
		name:    "attempt without activity",
		update:  requeststatus.Update{Phase: requeststatus.PhaseWaiting, Reason: "ci", Attempt: 1},
		wantErr: true,
	}, {
		name:    "activity without attempt",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "ci-fix", Activity: requeststatus.ActivityEnrich},
		wantErr: true,
	}, {
		name:    "negative attempt",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "ci-fix", Activity: requeststatus.ActivityEnrich, Attempt: -1},
		wantErr: true,
	}, {
		name:    "needs_you with empty reason",
		update:  requeststatus.Update{Phase: requeststatus.PhaseNeedsYou},
		wantErr: true,
	}, {
		name:    "failed with empty reason",
		update:  requeststatus.Update{Phase: requeststatus.PhaseFailed},
		wantErr: true,
	}, {
		name:    "no_action_needed with wrong reason",
		update:  requeststatus.Update{Outcome: requeststatus.OutcomeNoActionNeeded, Reason: "merged"},
		wantErr: true,
	}, {
		name:    "complete with wrong reason",
		update:  requeststatus.Update{Outcome: requeststatus.OutcomeComplete, Reason: "already-satisfied"},
		wantErr: true,
	}, {
		name:    "canceled with wrong reason",
		update:  requeststatus.Update{Outcome: requeststatus.OutcomeCanceled, Reason: "merged"},
		wantErr: true,
	}, {
		name:    "running with undeclared reason",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "bogus"},
		wantErr: true,
	}, {
		name:    "waiting with undeclared reason",
		update:  requeststatus.Update{Phase: requeststatus.PhaseWaiting, Reason: "bogus"},
		wantErr: true,
	}, {
		name:    "change link label without url",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Change: requeststatus.ChangeLink{Label: "PR #1"}},
		wantErr: true,
	}, {
		name:    "change link url without label",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Change: requeststatus.ChangeLink{URL: "https://github.com/example/repo/pull/1"}},
		wantErr: true,
	}, {
		name:    "change link with no host",
		update:  requeststatus.Update{Phase: requeststatus.PhaseRunning, Change: requeststatus.ChangeLink{Label: "no host", URL: "https:///etc/passwd"}},
		wantErr: true,
	}, {
		name: "change link non-https",
		update: requeststatus.Update{
			Phase:  requeststatus.PhaseRunning,
			Change: requeststatus.ChangeLink{Label: "PR #1", URL: "http://github.com/example/repo/pull/1"},
		},
		wantErr: true,
	}, {
		name: "change link undeclared host",
		update: requeststatus.Update{
			Phase:  requeststatus.PhaseRunning,
			Change: requeststatus.ChangeLink{Label: "MR !1", URL: "https://gitlab.com/example/repo/-/merge_requests/1"},
		},
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(tt.update)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidatorValidateChangeLinkHostCase pins case-insensitive host
// matching in both directions: declared casing and URL casing.
func TestValidatorValidateChangeLinkHostCase(t *testing.T) {
	tests := []struct {
		name    string
		declare string
		url     string
	}{{
		name:    "declared lower-case host, mixed-case URL",
		declare: "github.com",
		url:     "https://GitHub.com/example/repo/pull/1",
	}, {
		name:    "declared lower-case host, upper-case URL",
		declare: "github.com",
		url:     "https://GITHUB.COM/example/repo/pull/1",
	}, {
		name:    "declared mixed-case host, lower-case URL",
		declare: "GitHub.com",
		url:     "https://github.com/example/repo/pull/1",
	}, {
		name:    "declared mixed-case host, mixed-case URL",
		declare: "GitHub.com",
		url:     "https://GITHUB.com/example/repo/pull/1",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := mustValidator(t, requeststatus.WithLinkHosts(tt.declare))
			update := requeststatus.Update{
				Phase:  requeststatus.PhaseRunning,
				Change: requeststatus.ChangeLink{Label: "PR #1", URL: tt.url},
			}
			if err := v.Validate(update); err != nil {
				t.Fatalf("Validate() with declared host %q and URL %q: error = %v, want nil", tt.declare, tt.url, err)
			}
		})
	}

	// Case folding must not widen the declared set.
	v := mustValidator(t, requeststatus.WithLinkHosts("github.com"))
	update := requeststatus.Update{
		Phase:  requeststatus.PhaseRunning,
		Change: requeststatus.ChangeLink{Label: "MR !1", URL: "https://GitLab.com/example/repo/-/merge_requests/1"},
	}
	if err := v.Validate(update); err == nil {
		t.Fatalf("Validate() with undeclared mixed-case host: error = nil, want error")
	}
}

// TestValidatorValidateUndeclaredReasonsFailClosed proves that a phase with
// no declared reason set rejects any nonempty reason, rather than accepting
// it as unrestricted. Registering a different set for the same phase then
// accepts its own reasons, per the acceptance criteria's "a different
// configured set accepts its own reasons".
func TestValidatorValidateUndeclaredReasonsFailClosed(t *testing.T) {
	bare := mustValidator(t)
	if err := bare.Validate(requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "initial"}); err == nil {
		t.Fatalf("Validate() with no declared reasons: error = nil, want error")
	}

	configured := mustValidator(t, requeststatus.WithReasons(requeststatus.PhaseRunning, "custom-reason"))
	if err := configured.Validate(requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "custom-reason"}); err != nil {
		t.Fatalf("Validate() with declared reason: error = %v, want nil", err)
	}
	if err := configured.Validate(requeststatus.Update{Phase: requeststatus.PhaseRunning, Reason: "initial"}); err == nil {
		t.Fatalf("Validate() with reason outside declared set: error = nil, want error")
	}
}
