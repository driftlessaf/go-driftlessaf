/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus

import (
	"fmt"
	"net/url"
	"strings"
)

// Fixed reasons for terminal outcomes. Unlike Running and Waiting reasons,
// these are not configurable: every caller means the same thing by
// "already-satisfied", "merged", "request-closed", and
// "required-label-removed".
const (
	reasonAlreadySatisfied    = "already-satisfied"
	reasonMerged              = "merged"
	reasonRequestClosed       = "request-closed"
	reasonRequiredLabelRemove = "required-label-removed"
)

// Validate rejects an incoherent [Update]. It enforces, in order:
//
//   - Exactly one of Phase or Outcome is set, and any set Phase, Outcome,
//     or Activity is a known value.
//   - Activity is set only when Phase is [PhaseRunning].
//   - Activity and a positive Attempt are both set or both empty.
//   - [PhaseNeedsYou] and [PhaseFailed] carry a nonempty Reason.
//   - [OutcomeNoActionNeeded], [OutcomeComplete], and [OutcomeCanceled]
//     carry their fixed Reason.
//   - A Reason for [PhaseRunning] or [PhaseWaiting] is empty or in the set
//     declared for that phase with [WithReasons].
//   - Change.Label and Change.URL are both set or both empty, and a set
//     URL is HTTPS on a host declared with [WithLinkHosts].
func (v *Validator) Validate(u Update) error {
	if (u.Phase == "") == (u.Outcome == "") {
		return fmt.Errorf("exactly one of Phase or Outcome must be set, got Phase=%q Outcome=%q", u.Phase, u.Outcome)
	}
	if u.Phase != "" {
		if _, ok := validPhases[u.Phase]; !ok {
			return fmt.Errorf("unknown phase %q", u.Phase)
		}
	}
	if u.Outcome != "" {
		if _, ok := validOutcomes[u.Outcome]; !ok {
			return fmt.Errorf("unknown outcome %q", u.Outcome)
		}
	}
	if u.Activity != "" {
		if _, ok := validActivities[u.Activity]; !ok {
			return fmt.Errorf("unknown activity %q", u.Activity)
		}
		if u.Phase != PhaseRunning {
			return fmt.Errorf("activity %q requires phase %q, got Phase=%q Outcome=%q", u.Activity, PhaseRunning, u.Phase, u.Outcome)
		}
	}
	if u.Attempt < 0 {
		return fmt.Errorf("attempt must not be negative, got %d", u.Attempt)
	}
	if (u.Activity != "") != (u.Attempt > 0) {
		return fmt.Errorf("activity and a positive attempt must both be set or both be empty, got Activity=%q Attempt=%d", u.Activity, u.Attempt)
	}

	if (u.Phase == PhaseNeedsYou || u.Phase == PhaseFailed) && u.Reason == "" {
		return fmt.Errorf("phase %q requires a nonempty reason", u.Phase)
	}

	if err := validateOutcomeReason(u.Outcome, u.Reason); err != nil {
		return err
	}
	if err := v.validatePhaseReason(u.Phase, u.Reason); err != nil {
		return err
	}

	return v.validateChangeLink(u.Change)
}

// validateOutcomeReason enforces the fixed reason each terminal outcome
// requires.
func validateOutcomeReason(o Outcome, reason string) error {
	var want string
	switch o {
	case OutcomeNoActionNeeded:
		want = reasonAlreadySatisfied
	case OutcomeComplete:
		want = reasonMerged
	case OutcomeCanceled:
		if reason == reasonRequestClosed || reason == reasonRequiredLabelRemove {
			return nil
		}
		return fmt.Errorf("outcome %q requires reason %q or %q, got %q", o, reasonRequestClosed, reasonRequiredLabelRemove, reason)
	default:
		return nil
	}
	if reason != want {
		return fmt.Errorf("outcome %q requires reason %q, got %q", o, want, reason)
	}
	return nil
}

// validatePhaseReason enforces that a Running or Waiting reason, if set, is
// in the set declared for that phase with [WithReasons]. A phase with no
// declared set accepts no reason: [Validator] fails closed rather than
// accepting anything a caller forgot to declare.
func (v *Validator) validatePhaseReason(p Phase, reason string) error {
	if reason == "" || (p != PhaseRunning && p != PhaseWaiting) {
		return nil
	}
	set, ok := v.reasons[p]
	if !ok {
		return fmt.Errorf("no reasons declared for phase %q, got reason %q", p, reason)
	}
	if _, ok := set[reason]; !ok {
		return fmt.Errorf("reason %q is not declared for phase %q", reason, p)
	}
	return nil
}

// validateChangeLink enforces that Label and URL are both set or both
// empty, and that a set URL is HTTPS on a host declared with
// [WithLinkHosts].
func (v *Validator) validateChangeLink(c ChangeLink) error {
	if (c.Label == "") != (c.URL == "") {
		return fmt.Errorf("change link label and URL must both be set or both be empty, got Label=%q URL=%q", c.Label, c.URL)
	}
	if c.URL == "" {
		return nil
	}
	parsed, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("parsing change link URL %q: %w", c.URL, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("change link URL %q must use https", c.URL)
	}
	// url.Hostname preserves casing; DNS names are case-insensitive.
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("change link URL %q has no host", c.URL)
	}
	if _, ok := v.linkHosts[host]; !ok {
		return fmt.Errorf("change link host %q is not declared", host)
	}
	return nil
}
