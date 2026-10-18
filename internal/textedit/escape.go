/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package textedit

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// UnescapedNote is the note for an edit that applied only after old_string
// and new_string were decoded with UnescapeJSON.
const UnescapedNote = `old_string and new_string held JSON escape sequences such as \n, \t or \" as literal text, which the file does not have, so both were decoded once before the edit; send the file's own characters, not the escaped form they take inside a JSON string`

// jsonEscape matches one JSON escape sequence. A UTF-16 surrogate pair is one
// match so that it decodes to a single character.
var jsonEscape = regexp.MustCompile(`\\(?:u[dD][89abAB][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|["\\/bfnrt])`)

// UnescapeJSON decodes s as the body of a JSON string literal. ok is false
// when s between double quotes is not valid JSON: s holds a raw control
// character such as a newline or tab, an unescaped double quote, or a
// backslash that starts no JSON escape sequence.
func UnescapeJSON(s string) (string, bool) {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return "", false
	}
	return out, true
}

// UnescapeJSONSequences decodes every JSON escape sequence in s and keeps the
// rest of s as it is, so unlike UnescapeJSON it also decodes text that mixes
// escaped and raw characters, such as a raw newline beside an escaped quote.
func UnescapeJSONSequences(s string) string {
	return jsonEscape.ReplaceAllStringFunc(s, func(e string) string {
		d, _ := UnescapeJSON(e)
		return d
	})
}

// EscapedNotFound is the error for an old_string that matches nothing as
// written but matches once UnescapeJSONSequences decodes it. offsets holds the
// byte offsets of its first one or two decoded matches; two mean the decoded
// text also needs more context to be unique.
func EscapedNotFound(offsets []int64) error {
	where, unique := "it matches the file", ""
	switch {
	case len(offsets) == 1:
		where = fmt.Sprintf("it matches at byte offset %d", offsets[0])
	case len(offsets) > 1:
		where = fmt.Sprintf("it matches at byte offsets %d and %d", offsets[0], offsets[1])
		unique = ", with enough surrounding context to be unique"
	}
	return fmt.Errorf(`%s: it holds JSON escape sequences such as \n, \t or \" as literal text, and decoded %s; send old_string and new_string as the file's own characters%s, not the escaped form they take inside a JSON string`, notFound, where, unique)
}
