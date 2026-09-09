/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus

import (
	"fmt"
	"strings"
)

// Validator checks [Update] values for coherence before they reach a
// renderer or a [Surface]. Its zero value from [NewValidator] with no
// options accepts no Reason for [PhaseRunning] or [PhaseWaiting] and no
// [ChangeLink]: both are opt-in and fail closed until configured.
type Validator struct {
	reasons   map[Phase]map[string]struct{}
	linkHosts map[string]struct{}
}

// Option configures a [Validator].
type Option func(*Validator) error

// WithReasons declares the reasons [Validator.Validate] accepts for phase,
// which must be [PhaseRunning] or [PhaseWaiting]. Reasons for
// [PhaseNeedsYou] and [PhaseFailed] are owned by each bot's failure table
// and stay open: Validate only requires them to be nonempty, and
// registering them here is an error.
//
// Calling WithReasons for the same phase more than once replaces the
// previously declared set rather than adding to it.
func WithReasons(phase Phase, reasons ...string) Option {
	return func(v *Validator) error {
		if phase != PhaseRunning && phase != PhaseWaiting {
			return fmt.Errorf("reasons may only be declared for %q or %q, got %q", PhaseRunning, PhaseWaiting, phase)
		}
		set := make(map[string]struct{}, len(reasons))
		for _, r := range reasons {
			set[r] = struct{}{}
		}
		v.reasons[phase] = set
		return nil
	}
}

// WithLinkHosts declares the hostnames a [ChangeLink.URL] may target.
// Calling WithLinkHosts more than once adds to the declared set rather than
// replacing it. An empty hostname is rejected: a URL like "https:///path"
// parses with an empty [net/url.URL.Hostname], and declaring "" as a host
// would let that hostless URL through the allowlist it is meant to enforce.
// Hosts are compared case-insensitively.
func WithLinkHosts(hosts ...string) Option {
	return func(v *Validator) error {
		for _, h := range hosts {
			if h == "" {
				return fmt.Errorf("link host must not be empty")
			}
			v.linkHosts[strings.ToLower(h)] = struct{}{}
		}
		return nil
	}
}

// NewValidator builds a [Validator] from opts.
func NewValidator(opts ...Option) (*Validator, error) {
	v := &Validator{
		reasons:   make(map[Phase]map[string]struct{}),
		linkHosts: make(map[string]struct{}),
	}
	for _, opt := range opts {
		if err := opt(v); err != nil {
			return nil, err
		}
	}
	return v, nil
}
