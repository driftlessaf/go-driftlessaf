/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type validSchema struct{}

func (validSchema) PredicateType() string { return "https://status.cgr.dev/test/scan" }

type emptySchema struct{}

func (emptySchema) PredicateType() string { return "" }

// aliasSchema returns a key from cosign's predicate alias table, which secant
// rewrites on write while reads match the literal.
type aliasSchema struct{}

func (aliasSchema) PredicateType() string { return "spdx" }

type schemelessSchema struct{}

func (schemelessSchema) PredicateType() string { return "status.cgr.dev/test/scan" }

// pathOnlySchema passes url.ParseRequestURI but carries no scheme.
type pathOnlySchema struct{}

func (pathOnlySchema) PredicateType() string { return "/test/scan" }

func TestPredicateTypeOf(t *testing.T) {
	got, err := predicateTypeOf[validSchema]()
	require.NoError(t, err)
	require.Equal(t, "https://status.cgr.dev/test/scan", got)

	for _, tc := range []struct {
		name string
		call func() (string, error)
	}{
		{"empty", predicateTypeOf[emptySchema]},
		{"cosign alias", predicateTypeOf[aliasSchema]},
		{"missing scheme", predicateTypeOf[schemelessSchema]},
		{"path only", predicateTypeOf[pathOnlySchema]},
		// Both zero to nil, so the method call would panic.
		{"pointer instantiation", predicateTypeOf[*validSchema]},
		{"interface instantiation", predicateTypeOf[Predicated]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			require.Error(t, err)
		})
	}
}

// TestNewRejectsBadPredicateTypeBeforeNetwork checks the validation runs ahead
// of the TUF trusted-root fetch, so a bad schema fails without touching the
// network.
func TestNewRejectsBadPredicateTypeBeforeNetwork(t *testing.T) {
	_, err := New[aliasSchema](t.Context())
	require.ErrorContains(t, err, `predicate type "spdx"`)
}

// TestNewRejectsPointerInstantiation checks a pointer type parameter surfaces
// as an error through the public API rather than a panic.
func TestNewRejectsPointerInstantiation(t *testing.T) {
	_, err := NewReadOnly[*validSchema](t.Context())
	require.ErrorContains(t, err, "instantiate the Manager with the value type")
}
