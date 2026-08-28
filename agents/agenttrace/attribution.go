/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Attribution identifies the route that served an LLM turn. ProviderName is
// the canonical OpenTelemetry gen_ai.provider.name value. System retains the
// deprecated gen_ai.system value while historical consumers migrate.
// LogicalModel and Protocol identify the provider-independent model choice and
// the wire protocol selected by the router.
//
// Attribution contains only validated identifiers. It must never contain
// credentials, endpoints, or other request-specific data.
type Attribution struct {
	ProviderName string
	System       string
	LogicalModel string
	Protocol     string
}

// Validate reports whether all route attribution fields are present and safe
// to emit as telemetry attributes. Identifier-specific validation remains the
// router's responsibility; this check protects direct executor callers from
// emitting empty or control-character-bearing values.
func (a Attribution) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "provider name", value: a.ProviderName},
		{name: "system", value: a.System},
		{name: "logical model", value: a.LogicalModel},
		{name: "protocol", value: a.Protocol},
	} {
		if err := validateAttributionValue(field.value); err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
	}
	return nil
}

func validateAttributionValue(value string) error {
	if value == "" {
		return errors.New("cannot be empty")
	}
	if strings.TrimSpace(value) != value {
		return errors.New("cannot have leading or trailing whitespace")
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return errors.New("cannot contain control characters")
	}
	return nil
}
