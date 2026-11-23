/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

// ArgLogPolicy is an allowlist of the tool-call arguments an executor may write
// to a logging sink. Names and values both come from the model's JSON, so an
// argument not listed here is withheld whole and counted instead of logged.
//
// One tool declaring a policy polices every tool of the run. A run in which no
// tool declares one logs every argument unless its executor is configured with
// WithToolArgsWithheld.
type ArgLogPolicy struct {
	// Loggable names the arguments an executor may log. Nil or empty is valid
	// and maximally strict: it withholds every argument of the tool.
	Loggable []string
}

// NewArgLogPolicy returns a policy allowing only the named arguments.
func NewArgLogPolicy(loggable ...string) *ArgLogPolicy {
	return &ArgLogPolicy{Loggable: loggable}
}
