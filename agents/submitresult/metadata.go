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

// optionsForResponse builds Options from the annotations present on the response type T.
func optionsForResponse[T any]() Options[T] {
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

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
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
	for part := range strings.SplitSeq(tag, ",") {
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
		case "name", "tool", "toolname":
			meta.ToolName = value
		case "description", "toolDescription":
			meta.Description = value
		case "success", "successmessage":
			meta.SuccessMessage = value
		case "payload", "payloadfield", "payloadfieldname":
			meta.PayloadFieldName = value
		case "payloaddescription", "payload_desc":
			meta.PayloadDescription = value
		}
	}
}

// OptionsForResponse returns an Options pre-populated from the annotations present on
// the response type T. Callers may further customize the returned struct before passing
// it to ClaudeTool or GoogleTool.
func OptionsForResponse[T any]() Options[T] {
	return optionsForResponse[T]()
}
