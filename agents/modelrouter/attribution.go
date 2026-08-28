/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter

import (
	"errors"
	"fmt"
)

// ErrInvalidAttribution identifies empty or malformed route attribution.
var ErrInvalidAttribution = errors.New("invalid route attribution")

// Attribution declares the two provider-controlled telemetry identities for a
// route. ProviderName is the canonical gen_ai.provider.name value.
// LegacySystem is the deprecated gen_ai.system compatibility value retained
// while telemetry consumers migrate.
//
// Logical model, protocol, and exact provider model ID remain authoritative on
// Plan and are intentionally not duplicated here.
type Attribution struct {
	ProviderName string
	LegacySystem string
}

// Validate verifies that a contains usable, secret-free provider identifiers.
func (a Attribution) Validate() error {
	if err := Provider(a.ProviderName).Validate(); err != nil {
		return fmt.Errorf("%w: provider name: %w", ErrInvalidAttribution, err)
	}
	if err := Provider(a.LegacySystem).Validate(); err != nil {
		return fmt.Errorf("%w: legacy system: %w", ErrInvalidAttribution, err)
	}
	return nil
}
