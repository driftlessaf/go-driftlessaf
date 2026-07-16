/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// bqColumn is the minimum shape of a BQ schema entry — name+type are enough
// to verify that the JSON tags on RecordedSpan and the schema's columns line
// up. Mode is ignored here; the file is hand-maintained for REQUIRED markers.
type bqColumn struct {
	Name   string     `json:"name"`
	Type   string     `json:"type"`
	Mode   string     `json:"mode"`
	Fields []bqColumn `json:"fields"`
}

// TestAgentTraceSpanSchemaMatchesStruct guards against drift between the
// agent_trace_span BigQuery schema and the RecordedSpan Go struct. Any
// added/removed/renamed JSON tag must be mirrored in the schema or the BQ
// recorder will silently drop the column.
func TestAgentTraceSpanSchemaMatchesStruct(t *testing.T) {
	raw, err := os.ReadFile("iac/schemas/agent_trace_span.schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var cols []bqColumn
	if err := json.Unmarshal(raw, &cols); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	gotCols := make([]string, 0, len(cols))
	for _, c := range cols {
		gotCols = append(gotCols, c.Name)
	}
	sort.Strings(gotCols)

	wantCols := jsonTags(reflect.TypeFor[RecordedSpan]())
	sort.Strings(wantCols)

	if !reflect.DeepEqual(gotCols, wantCols) {
		t.Errorf("schema columns differ from RecordedSpan JSON tags:\nschema: %v\nstruct: %v", gotCols, wantCols)
	}
}

// TestAgentTraceSchemaMatchesStruct guards against drift between the
// agent_trace BigQuery schema and the JSON shape Trace marshals to. Since
// Trace.MarshalJSON reuses the struct tags via the alias pattern, a new
// struct field auto-serializes — this test ensures it cannot land without a
// matching schema column, or the BQ recorder would silently drop it. The
// check is one-directional (struct ⊆ schema) because BigQuery cannot drop
// columns: the schema legitimately retains legacy columns with no struct
// counterpart (the trace-level token counts removed in DEV-1140 and the
// singular turns[].error).
func TestAgentTraceSchemaMatchesStruct(t *testing.T) {
	raw, err := os.ReadFile("iac/schemas/agent_trace.schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var cols []bqColumn
	if err := json.Unmarshal(raw, &cols); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	fieldsOf := func(name string) []bqColumn {
		for _, c := range cols {
			if c.Name == name {
				return c.Fields
			}
		}
		t.Fatalf("schema has no %q record", name)
		return nil
	}

	tests := []struct {
		name string
		tags []string
		cols []bqColumn
		// knownDrift lists struct fields the schema is known to lack: the
		// recorder drops them today. Entries here document pre-existing gaps,
		// not a license to add more — new fields need schema columns.
		knownDrift []string
	}{{
		name: "trace",
		// "error" comes from the MarshalJSON override of the json:"-" field.
		tags: append(jsonTags(reflect.TypeFor[Trace[string]]()), "error"),
		cols: cols,
	}, {
		name: "tool_calls",
		tags: append(jsonTags(reflect.TypeFor[ToolCall[string]]()), "error"),
		cols: fieldsOf("tool_calls"),
	}, {
		name: "turns",
		tags: jsonTags(reflect.TypeFor[RecordedTurn]()),
		cols: fieldsOf("turns"),
	}, {
		name: "reasoning",
		tags: jsonTags(reflect.TypeFor[ReasoningContent]()),
		cols: fieldsOf("reasoning"),
	}, {
		name:       "exec_context",
		tags:       jsonTags(reflect.TypeFor[ExecutionContext]()),
		cols:       fieldsOf("exec_context"),
		knownDrift: []string{"request_id", "labels"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			have := make(map[string]struct{}, len(tt.cols))
			for _, c := range tt.cols {
				have[c.Name] = struct{}{}
			}
			drift := make(map[string]struct{}, len(tt.knownDrift))
			for _, d := range tt.knownDrift {
				drift[d] = struct{}{}
				if _, ok := have[d]; ok {
					t.Errorf("field %q now has a schema column — remove it from knownDrift", d)
				}
			}
			for _, tag := range tt.tags {
				_, inSchema := have[tag]
				_, inDrift := drift[tag]
				if !inSchema && !inDrift {
					t.Errorf("marshaled field %q has no schema column — the BQ recorder will drop it", tag)
				}
			}
		})
	}
}

// jsonTags returns the JSON tag names (with omitempty stripped) for all
// fields of t.
func jsonTags(t reflect.Type) []string {
	fields := reflect.VisibleFields(t)
	tags := make([]string, 0, len(fields))
	for _, field := range fields {
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// strip ",omitempty" and friends
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		tags = append(tags, tag)
	}
	return tags
}
