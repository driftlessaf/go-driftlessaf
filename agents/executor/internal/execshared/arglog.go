/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"chainguard.dev/driftlessaf/agents/toolcall"
)

// WithheldArgCountKey is the log key carrying how many of a call's arguments a
// policed run withheld.
const WithheldArgCountKey = "args_withheld"

// UnknownToolName is what a policed run logs in place of a call name it never
// registered, so an invented name cannot carry its text into the "tool" key.
const UnknownToolName = "[unknown]"

// ArgLogRedactor applies a run's tool-call argument log policies to the per-call
// "Executing tool call" line. The zero value is unpoliced and logs every
// argument, which is what every run did before policies existed.
type ArgLogRedactor struct {
	// policed is true when any tool declared a policy. It gates the whole run,
	// so a tool added later without one is withheld rather than logged.
	policed  bool
	loggable map[string]map[string]struct{}
	// known names every tool the run serves. A call naming anything else took
	// its name from the model alone, so ToolName replaces it.
	known map[string]struct{}
}

// NewArgLogRedactor builds the redactor from a run's per-tool policies. A tool
// absent from policies, or mapped to nil, declared none. heldOut names the
// tools the run serves outside the policy map, such as submit and suspend.
func NewArgLogRedactor(policies map[string]*toolcall.ArgLogPolicy, heldOut ...string) ArgLogRedactor {
	r := ArgLogRedactor{known: make(map[string]struct{}, len(policies)+len(heldOut))}
	for name, policy := range policies {
		r.known[name] = struct{}{}
		if policy == nil {
			continue
		}
		r.policed = true
		if r.loggable == nil {
			r.loggable = make(map[string]map[string]struct{}, len(policies))
		}
		allow := make(map[string]struct{}, len(policy.Loggable))
		for _, arg := range policy.Loggable {
			allow[arg] = struct{}{}
		}
		r.loggable[name] = allow
	}
	for _, name := range heldOut {
		r.known[name] = struct{}{}
	}
	return r
}

// ToolName returns the name to log for a call. The model chooses the name, so
// a policed run replaces one it never registered. An unpoliced run logs it
// unchanged, which is what every run did before policies existed.
func (r ArgLogRedactor) ToolName(name string) string {
	if !r.policed {
		return name
	}
	if _, ok := r.known[name]; ok {
		return name
	}
	return UnknownToolName
}

// AppendArgs appends tool's call arguments to kvs as "args.<name>", value pairs.
// Names and values both come from the model's JSON, so a policed run appends an
// argument only when the policy allows it and counts the rest. Unpoliced appends all.
func (r ArgLogRedactor) AppendArgs(kvs []any, tool string, args map[string]any) []any {
	if !r.policed {
		for name, value := range args {
			kvs = append(kvs, "args."+name, value)
		}
		return kvs
	}
	allow := r.loggable[tool]
	withheld := 0
	for name, value := range args {
		if _, ok := allow[name]; ok {
			kvs = append(kvs, "args."+name, value)
			continue
		}
		withheld++
	}
	if withheld > 0 {
		kvs = append(kvs, WithheldArgCountKey, withheld)
	}
	return kvs
}
