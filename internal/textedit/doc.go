/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package textedit supports file-editing tools exposed to LLM agents.
//
// An agent's edit_file call names the text to replace by exact bytes. The most
// common way such a call misses is a constant indentation shift: every line of
// old_string matches the file once leading whitespace is ignored, but the file
// carries one more (or one fewer) tab on each line. MatchIgnoringIndentation
// finds the unique region that matches under that relaxation and reports the
// shift so the caller can apply the same shift to new_string with
// ShiftIndentation.
//
// A related source of misses is a read window that starts mid-line, which
// hands the agent a first line with its indentation cut off. LineStart and
// LineEnd align a byte window to whole lines.
package textedit
