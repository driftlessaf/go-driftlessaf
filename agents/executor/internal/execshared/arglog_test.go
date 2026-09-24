/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall"
)

// argPairs turns the alternating key/value slice AppendArgs produces into a map,
// failing on an odd length or a non-string key — the shape a clog call requires.
func argPairs(t *testing.T, kvs []any) map[string]any {
	t.Helper()
	if len(kvs)%2 != 0 {
		t.Fatalf("kvs length: got = %d, want even (clog reads alternating key/value)", len(kvs))
	}
	out := make(map[string]any, len(kvs)/2)
	for i := 0; i < len(kvs); i += 2 {
		key, ok := kvs[i].(string)
		if !ok {
			t.Fatalf("kvs[%d]: got = %T, want a string key", i, kvs[i])
		}
		out[key] = kvs[i+1]
	}
	return out
}

// TestArgLogRedactor_AppendArgs pins the policy semantics case by case. The
// executor tests drive these same rules through a real run; this table states
// each rule on its own so a failure names the rule that broke.
func TestArgLogRedactor_AppendArgs(t *testing.T) {
	t.Parallel()

	const (
		secret = "restricted-input-7742"
		ref    = "4.1.0"
	)
	args := map[string]any{"ref": ref, "pattern": secret, "reasoning": secret}

	for name, tc := range map[string]struct {
		policies map[string]*toolcall.ArgLogPolicy
		tool     string
		want     map[string]any
	}{
		// No tool declares a policy, so the run keeps the behavior it had
		// before policies existed. This is every agent that has not opted in.
		"unpoliced run logs every value": {
			policies: map[string]*toolcall.ArgLogPolicy{"git_grep": nil},
			tool:     "git_grep",
			want:     map[string]any{"args.ref": ref, "args.pattern": secret, "args.reasoning": secret},
		},
		// A withheld argument is COUNTED, not named: the name comes from the
		// model's JSON, so logging it would carry whatever the model wrote.
		"policy keeps only the named argument": {
			policies: map[string]*toolcall.ArgLogPolicy{
				"git_grep": toolcall.NewArgLogPolicy("ref"),
			},
			tool: "git_grep",
			want: map[string]any{"args.ref": ref, WithheldArgCountKey: 2},
		},
		// A nil or empty Loggable is a valid, maximally strict policy rather
		// than an accident that means "unpoliced".
		"empty policy withholds every value": {
			policies: map[string]*toolcall.ArgLogPolicy{
				"git_grep": {},
			},
			tool: "git_grep",
			want: map[string]any{WithheldArgCountKey: 3},
		},
		// The fail-closed default: a sibling declared a policy, so this tool is
		// withheld even though it declared none of its own.
		"unpoliced tool in a policed run is withheld": {
			policies: map[string]*toolcall.ArgLogPolicy{
				"git_grep":       toolcall.NewArgLogPolicy("ref"),
				"git_log_search": nil,
			},
			tool: "git_log_search",
			want: map[string]any{WithheldArgCountKey: 3},
		},
		// The held-out submit and suspend tools dispatch from outside the tool
		// map, as does a name the model invented. In a policed run all three
		// resolve to no policy and are withheld.
		"tool absent from the map is withheld": {
			policies: map[string]*toolcall.ArgLogPolicy{
				"git_grep": toolcall.NewArgLogPolicy("ref"),
			},
			tool: "submit_result",
			want: map[string]any{WithheldArgCountKey: 3},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			redactor := NewArgLogRedactor(tc.policies)
			got := argPairs(t, redactor.AppendArgs(nil, tc.tool, args))

			// An exact count is the assertion that matters: a withheld argument
			// must contribute NO key of its own, so an extra pair here means a
			// model-chosen name reached the line.
			if len(got) != len(tc.want) {
				t.Errorf("logged pairs: got = %d (%v), want = %d", len(got), got, len(tc.want))
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("%s: got = %v, want = %v", key, got[key], want)
				}
			}
		})
	}
}

// The arguments arrive as the model's raw JSON, so the model chooses the keys.
// A policed run must not log a key it did not declare, whatever the value.
func TestArgLogRedactor_InventedArgumentNameIsNotLogged(t *testing.T) {
	t.Parallel()

	const injected = "the caller input names lib/index.ts"

	redactor := NewArgLogRedactor(map[string]*toolcall.ArgLogPolicy{"git_grep": toolcall.NewArgLogPolicy("ref")})

	got := argPairs(t, redactor.AppendArgs(nil, "git_grep",
		map[string]any{"ref": "4.1.0", injected: "1"}))

	for key, value := range got {
		if strings.Contains(key, injected) {
			t.Errorf("log key %q carries the invented argument name", key)
		}
		if text, ok := value.(string); ok && strings.Contains(text, injected) {
			t.Errorf("log value %s = %q carries the invented argument name", key, text)
		}
	}
	if got["args.ref"] != "4.1.0" {
		t.Errorf("args.ref: got = %v, want = %q (the declared argument stays legible)", got["args.ref"], "4.1.0")
	}
	if got[WithheldArgCountKey] != 1 {
		t.Errorf("%s: got = %v, want = 1", WithheldArgCountKey, got[WithheldArgCountKey])
	}
}

// TestArgLogRedactor_ToolName pins the "tool" key. The provider reports the name
// the model emitted, so an unregistered one is model text and is replaced rather
// than logged.
func TestArgLogRedactor_ToolName(t *testing.T) {
	t.Parallel()

	// A name is a plausible injection sink: the model can spell a whole sentence
	// as the function it claims to call.
	const injected = "the caller input names lib/index.ts"

	policed := map[string]*toolcall.ArgLogPolicy{
		"git_grep":       toolcall.NewArgLogPolicy("ref"),
		"git_log_search": nil,
	}

	for name, tc := range map[string]struct {
		policies map[string]*toolcall.ArgLogPolicy
		heldOut  []string
		call     string
		want     string
	}{
		"registered tool keeps its name": {
			policies: policed, call: "git_grep", want: "git_grep",
		},
		// A tool with no policy of its own is still a real tool, so the name
		// stays legible even though every argument is withheld.
		"unpoliced tool in a policed run keeps its name": {
			policies: policed, call: "git_log_search", want: "git_log_search",
		},
		// Submit and suspend dispatch from outside the policy map. Losing their
		// names would cost an operator the two calls that end a run.
		"held-out tool keeps its name": {
			policies: policed, heldOut: []string{"submit_result"}, call: "submit_result", want: "submit_result",
		},
		"invented name is replaced": {
			policies: policed, call: injected, want: UnknownToolName,
		},
		// The name follows the same switch as the arguments: a run that declares
		// no policy logs what it logged before policies existed.
		"unpoliced run keeps an invented name": {
			policies: map[string]*toolcall.ArgLogPolicy{"git_grep": nil}, call: injected, want: injected,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := NewArgLogRedactor(tc.policies, tc.heldOut...).ToolName(tc.call)
			if got != tc.want {
				t.Errorf("ToolName(%q): got = %q, want = %q", tc.call, got, tc.want)
			}
		})
	}
}

// The zero value is unpoliced. Stating it keeps a refactor from making the zero
// value strict and silently dropping every agent's tool-call visibility.
func TestArgLogRedactor_ZeroValueIsUnpoliced(t *testing.T) {
	t.Parallel()

	var redactor ArgLogRedactor
	got := argPairs(t, redactor.AppendArgs(nil, "git_grep", map[string]any{"ref": "4.1.0"}))
	if got["args.ref"] != "4.1.0" {
		t.Errorf("args.ref: got = %v, want = %q", got["args.ref"], "4.1.0")
	}
}

// TestArgLogRedactor_PreservesCallerKVs proves AppendArgs appends rather than
// replaces: the executor passes it a kvs slice already holding the tool name and
// call id, and losing those would strip the fields an operator correlates on.
func TestArgLogRedactor_PreservesCallerKVs(t *testing.T) {
	t.Parallel()

	redactor := NewArgLogRedactor(map[string]*toolcall.ArgLogPolicy{"git_grep": toolcall.NewArgLogPolicy("ref")})

	kvs := []any{"tool", "git_grep", "id", "toolu_01"}
	got := argPairs(t, redactor.AppendArgs(kvs, "git_grep", map[string]any{"pattern": "restricted-input-7742"}))

	if got["tool"] != "git_grep" || got["id"] != "toolu_01" {
		t.Errorf("caller kvs: got = %v, want tool=git_grep id=toolu_01", got)
	}
	if got[WithheldArgCountKey] != 1 {
		t.Errorf("%s: got = %v, want = 1", WithheldArgCountKey, got[WithheldArgCountKey])
	}
}
