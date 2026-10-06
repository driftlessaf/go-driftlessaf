/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	// minReducibleBytes keeps small results whole: shortening them saves
	// little and costs the model a call to fetch them again.
	minReducibleBytes = 4096
	// reducedEdgeBytes is the length of the head and the tail kept from a
	// reduced result.
	reducedEdgeBytes = 1024
	// maxReducedFields and maxReducedFieldBytes bound the top-level scalar
	// fields a reduced result keeps, which carry its paging offsets.
	maxReducedFields     = 16
	maxReducedFieldBytes = 256
	// maxFindingBytes bounds the text of each nested finding a reduced
	// result keeps, and maxFindingsBytes bounds the encoded findings list.
	maxFindingBytes  = 512
	maxFindingsBytes = 2048
	// maxFindingPathBytes bounds the JSON path kept with each finding.
	maxFindingPathBytes = 256
	// bytesPerTokenEstimate converts saved bytes into tokens when deciding
	// whether enough has been reduced. Every decision to stop is checked by
	// an exact recount, so the estimate only sets how much is reduced first.
	bytesPerTokenEstimate = 4
)

// reducedResult is the JSON that replaces a reduced tool result. The tool
// name and tool_use_id point at the tool_use block that stays in the
// conversation, so the model can call the tool again with the same input.
type reducedResult struct {
	ExecutorReduced bool                       `json:"executor_reduced"`
	Tool            string                     `json:"tool"`
	ToolUseID       string                     `json:"tool_use_id"`
	OriginalBytes   int                        `json:"original_bytes"`
	Fields          map[string]json.RawMessage `json:"fields,omitempty"`
	Findings        []reducedFinding           `json:"findings,omitempty"`
	Head            string                     `json:"head"`
	Tail            string                     `json:"tail"`
	Note            string                     `json:"note"`
}

// reducedFinding is one string a reduced result keeps from beneath a
// findingKeys key, at its JSON path in the original result.
type reducedFinding struct {
	Path      string `json:"path"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// findingKeys name the keys whose strings, at any depth beneath them, a
// reduced result keeps. A result such as a nested log analysis can cost a model call to
// produce again, so its error text and summary must survive reduction even
// though the note points at the tool_use.
var findingKeys = map[string]struct{}{
	"error":         {},
	"errors":        {},
	"error_message": {},
	"findings":      {},
	"summary":       {},
}

// reduceToolResults shortens eligible tool results that come before the last
// assistant message, oldest first, and returns how many it shortened. Unless
// all is set it stops once the estimated saving covers excessTokens.
//
// Results after the last assistant message are skipped because the model has
// not read them yet. Results are also kept whole when they are not text
// only, are smaller than minReducibleBytes, are already reduced, are errors
// (IsError or a top-level "error" key), have no paired tool_use, or pair with
// a tool protected reports true for.
//
// Each reduced block is replaced at the same message and block index and
// keeps its ToolUseID, IsError and CacheControl, so tool_use pairing and the
// cache tail positions stay valid.
func reduceToolResults(messages []anthropic.MessageParam, protected func(name string) bool, excessTokens int64, all bool) (reduced int) {
	last := -1
	for i, m := range slices.Backward(messages) {
		if m.Role == anthropic.MessageParamRoleAssistant {
			last = i
			break
		}
	}
	if last < 0 {
		return 0
	}
	older := messages[:last]

	names := make(map[string]string)
	for _, m := range older {
		if m.Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.OfToolUse != nil {
				names[b.OfToolUse.ID] = b.OfToolUse.Name
			}
		}
	}

	var saved int64
	for _, m := range older {
		if m.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for b := range m.Content {
			if !all && saved/bytesPerTokenEstimate >= excessTokens {
				return reduced
			}
			tr := m.Content[b].OfToolResult
			if tr == nil || (tr.IsError.Valid() && tr.IsError.Value) {
				continue
			}
			name, ok := names[tr.ToolUseID]
			if !ok || protected(name) {
				continue
			}
			text, ok := toolResultText(tr)
			if !ok || len(text) < minReducibleBytes {
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(text), &obj) != nil {
				obj = nil
			}
			if _, isErr := obj["error"]; isErr {
				continue
			}
			if _, done := obj["executor_reduced"]; done {
				continue
			}
			replacement, ok := reducedResultText(name, tr.ToolUseID, text, obj)
			if !ok || len(replacement) >= len(text) {
				continue
			}
			nb := *tr
			nb.Content = []anthropic.ToolResultBlockParamContentUnion{{
				OfText: &anthropic.TextBlockParam{Text: replacement},
			}}
			m.Content[b].OfToolResult = &nb
			saved += int64(len(text) - len(replacement))
			reduced++
		}
	}
	return reduced
}

// toolResultText joins the text of a tool result whose content is text only.
func toolResultText(tr *anthropic.ToolResultBlockParam) (string, bool) {
	if len(tr.Content) == 0 {
		return "", false
	}
	var sb strings.Builder
	for _, c := range tr.Content {
		if c.OfText == nil {
			return "", false
		}
		sb.WriteString(c.OfText.Text)
	}
	return sb.String(), true
}

// reducedResultText renders the replacement for a tool result of tool with
// the given text, whose top-level JSON object is obj (nil when the text is
// not an object). Head and tail are cut on rune boundaries.
func reducedResultText(tool, toolUseID, text string, obj map[string]json.RawMessage) (string, bool) {
	head := reducedEdgeBytes
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	tail := len(text) - reducedEdgeBytes
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	fields := scalarFields(obj)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The head and tail are mostly JSON already, so HTML escaping would
	// only inflate them.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(reducedResult{
		ExecutorReduced: true,
		Tool:            tool,
		ToolUseID:       toolUseID,
		OriginalBytes:   len(text),
		Fields:          fields,
		Findings:        findingStrings(obj, fields),
		Head:            text[:head],
		Tail:            text[tail:],
		Note: fmt.Sprintf("The executor shortened this result to fit the model input budget. "+
			"To read all of it, call %s again with the input of tool_use %s. "+
			"fields keeps the result's top-level scalar values, such as paging offsets, "+
			"and findings keeps the start of its error and summary text.", tool, toolUseID),
	}); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// scalarFields returns the first maxReducedFields top-level numbers, booleans
// and short strings of obj in key order.
func scalarFields(obj map[string]json.RawMessage) map[string]json.RawMessage {
	keys := slices.Sorted(maps.Keys(obj))
	var fields map[string]json.RawMessage
	for _, k := range keys {
		if len(fields) == maxReducedFields {
			break
		}
		v := bytes.TrimSpace(obj[k])
		if len(v) == 0 {
			continue
		}
		switch c := v[0]; {
		case c == '"':
			var s string
			if json.Unmarshal(v, &s) != nil || len(s) > maxReducedFieldBytes {
				continue
			}
		case c == 't', c == 'f', c == '-', c >= '0' && c <= '9':
		default:
			continue
		}
		if fields == nil {
			fields = make(map[string]json.RawMessage, maxReducedFields)
		}
		fields[k] = v
	}
	return fields
}

// findingStrings returns the strings of obj anywhere beneath a findingKeys
// key, in key order, skipping the top-level keys that fields keeps. Each text
// is cut to maxFindingBytes and each path to maxFindingPathBytes. The list
// stops when its JSON encoding would pass maxFindingsBytes.
func findingStrings(obj map[string]json.RawMessage, fields map[string]json.RawMessage) []reducedFinding {
	var out []reducedFinding
	// The encoded list is "[", the entries joined by ",", and "]".
	total := len("[]")
	var walk func(v any, path string, under bool) bool
	walk = func(v any, path string, under bool) bool {
		switch v := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(v)) {
				_, isKey := findingKeys[k]
				if !walk(v[k], path+"."+k, under || isKey) {
					return false
				}
			}
		case []any:
			for i, e := range v {
				if !walk(e, fmt.Sprintf("%s[%d]", path, i), under) {
					return false
				}
			}
		case string:
			if !under || v == "" {
				return true
			}
			sep := 0
			if len(out) > 0 {
				sep = len(",")
			}
			f, size := fitFinding(cutRunes(path, maxFindingPathBytes), v, maxFindingsBytes-total-sep)
			if size == 0 {
				return false
			}
			out = append(out, f)
			total += sep + size
		}
		return true
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if _, kept := fields[k]; kept {
			continue
		}
		var v any
		if json.Unmarshal(obj[k], &v) != nil {
			continue
		}
		_, isKey := findingKeys[k]
		if !walk(v, k, isKey) {
			break
		}
	}
	return out
}

// fitFinding returns the finding for text at path, with text cut until the
// entry encodes to at most budget bytes, and its encoded size. The size is
// zero when no non-empty cut fits.
func fitFinding(path, text string, budget int) (reducedFinding, int) {
	n := min(len(text), maxFindingBytes)
	for {
		cut := cutRunes(text, n)
		if cut == "" {
			return reducedFinding{}, 0
		}
		f := reducedFinding{Path: path, Text: cut, Truncated: len(cut) < len(text)}
		size := encodedLen(f)
		if size <= budget {
			return f, size
		}
		// Escaped characters encode longer than they are, so one cut can
		// fall short. n shrinks on every pass.
		n = len(cut) - (size - budget)
	}
}

// cutRunes returns the longest prefix of s of at most n bytes that ends on a
// rune boundary.
func cutRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	n = max(n, 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// encodedLen is the length of v encoded as reducedResultText encodes it.
func encodedLen(v any) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return 0
	}
	return len(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}
