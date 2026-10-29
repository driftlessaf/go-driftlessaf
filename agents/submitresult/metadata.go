/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"reflect"
	"strings"
)

// tagKey is the struct tag key used to configure submit_result metadata on response types.
const tagKey = "submitresult"

// tagMetadata captures struct-level metadata used to configure the submit_result tool.
type tagMetadata struct {
	ToolName           string
	Description        string
	SuccessMessage     string
	PayloadFieldName   string
	PayloadDescription string
}

// OptionsForResponse returns an Options pre-populated from the annotations present on
// the response type T. Callers may further customize the returned struct before passing
// it to ClaudeTool or GoogleTool.
func OptionsForResponse[T any]() Options[T] {
	meta, _ := extractMetadata(reflect.TypeFor[T]())
	return Options[T]{
		ToolName:           meta.ToolName,
		Description:        meta.Description,
		SuccessMessage:     meta.SuccessMessage,
		PayloadFieldName:   meta.PayloadFieldName,
		PayloadDescription: meta.PayloadDescription,
	}
}

// extractMetadata parses the submit_result annotations from the provided type.
func extractMetadata(t reflect.Type) (tagMetadata, bool) {
	meta := tagMetadata{}
	if t == nil {
		return meta, false
	}

	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return meta, false
	}

	for field := range t.Fields() {
		tag := field.Tag.Get(tagKey)
		if tag == "" {
			continue
		}

		parseTag(tag, &meta)
		return meta, true
	}

	return meta, false
}

func parseTag(tag string, meta *tagMetadata) {
	for _, part := range splitTagParts(tag) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		kv := strings.SplitN(part, "=", 2)
		key := strings.ToLower(strings.TrimSpace(kv[0]))

		var value string
		if len(kv) == 2 {
			value = strings.TrimSpace(kv[1])
		}

		switch key {
		case "name":
			meta.ToolName = value
		case "description":
			meta.Description = value
		case "success":
			meta.SuccessMessage = value
		case "payload":
			meta.PayloadFieldName = value
		case "payloaddescription":
			meta.PayloadDescription = value
		}
	}
}

// splitTagParts splits a tag on its unescaped commas. A comma preceded by a
// backslash (`\,` in the tag as reflect returns it, written `\\,` inside a
// raw-string struct tag) is part of the value, not a delimiter, and the
// escape is removed. Descriptions read by the model routinely need commas —
// "escalate, block, reasoning" — and a naive split silently truncates them
// at the first one, which the model sees as a tool it cannot use correctly.
func splitTagParts(tag string) []string {
	var (
		parts []string
		cur   strings.Builder
	)
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c == '\\' && i+1 < len(tag) && tag[i+1] == ',':
			cur.WriteByte(',')
			i++
		case c == ',':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	return parts
}
