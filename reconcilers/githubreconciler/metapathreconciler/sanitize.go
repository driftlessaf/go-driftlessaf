/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"fmt"
	"regexp"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

const (
	// analyzerSource tags the boundary element wrapped around the details of a
	// finding built from an analyzer diagnostic.
	analyzerSource = "analyzer"
	// maxDiagnosticMessageBytes bounds a diagnostic message before it becomes a
	// finding, so source-derived text cannot flood the agent's request.
	maxDiagnosticMessageBytes = 512
	// maxDiagnosticRuleBytes bounds a diagnostic rule id. Normal rule ids are far
	// shorter, so the cap never alters one; it bounds a pathological
	// source-derived value while keeping the id usable as a grouping key.
	maxDiagnosticRuleBytes = 128
	// maxDiagnosticPathBytes bounds a diagnostic path. A repo-relative path to a
	// file that exists in the checkout fits within the Linux PATH_MAX, so the
	// cap never alters one; it bounds a pathological source-derived value that
	// AsFinding copies into the identifier and name.
	maxDiagnosticPathBytes = 4096
	// maxFindingDetailsBytes bounds a finding's details after conversion, as
	// defense in depth after the message and rule caps.
	maxFindingDetailsBytes = 4096
	// truncationMarker is appended to any value cut at its byte cap.
	truncationMarker = "[truncated]"
)

var (
	// newlineRunRE matches a run of carriage returns and line feeds. Collapsing
	// each run to one space keeps a multi-line diagnostic from injecting line
	// breaks into the agent's request.
	newlineRunRE = regexp.MustCompile(`[\r\n]+`)
	// controlRunRE matches a run of characters that render as nothing or as a
	// line break: Unicode control (Cc, the C0 and C1 sets, NEL included),
	// format (Cf: bidirectional controls, zero-width characters, the byte
	// order mark), and the line and paragraph separators (Zl, Zp). A
	// diagnostic path is collapsed with it rather than newlineRunRE because a
	// path has no legitimate use for any of them, while a message may carry
	// tabs from a formatting diff.
	controlRunRE = regexp.MustCompile(`[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]+`)
	// untrustedMarkerRE matches the boundary tag in either open or close form so
	// diagnostic text cannot reproduce the wrapper it is placed inside.
	untrustedMarkerRE = regexp.MustCompile(`(?i)</?untrusted-content`)
)

// AsSanitizedFinding converts the diagnostic like [Diagnostic.AsFinding] but
// treats its text as source-derived data the change under review controls:
// control and newline runs in the path collapse to one space and any boundary
// marker in it is neutralized, since AsFinding copies the path into the
// identifier and name that sit outside the wrapper; newline runs in the message
// and rule collapse; each field is bounded; and the details are enclosed in an
// untrusted-content element tagged with source, so an identifier, literal, or
// path quoted by a tool reads to the model as data rather than instructions.
// A normal short rule id is left intact, so callers that group findings by rule
// still can.
func (d Diagnostic) AsSanitizedFinding(source string) callbacks.Finding {
	f := sanitizeDiagnostic(d).AsFinding()
	f.Details = wrapUntrusted(source, capBytes(f.Details, maxFindingDetailsBytes))
	return f
}

// sanitizeDiagnostic collapses control and newline runs and bounds the path,
// message, and rule of a diagnostic before it is converted to a finding.
func sanitizeDiagnostic(d Diagnostic) Diagnostic {
	d.Path = capBytes(neutralizeMarkers(controlRunRE.ReplaceAllString(d.Path, " ")), maxDiagnosticPathBytes)
	d.Message = capBytes(newlineRunRE.ReplaceAllString(d.Message, " "), maxDiagnosticMessageBytes)
	d.Rule = capBytes(newlineRunRE.ReplaceAllString(d.Rule, " "), maxDiagnosticRuleBytes)
	return d
}

// wrapUntrusted encloses body in a boundary element tagged with source,
// neutralizing any boundary tag inside body so it cannot forge the enclosing
// markers once the finding is serialized to XML.
func wrapUntrusted(source, body string) string {
	return fmt.Sprintf("<untrusted-content source=%q>\n%s\n</untrusted-content>", source, neutralizeMarkers(body))
}

// neutralizeMarkers rewrites every boundary tag in s so the text can neither
// close nor reopen an untrusted-content wrapper. The rewrite is loud rather
// than lossy: an attempted boundary stays legible as evidence.
func neutralizeMarkers(s string) string {
	return untrustedMarkerRE.ReplaceAllString(s, "<neutralized-untrusted-content")
}

// capBytes returns s unchanged when it fits within maxBytes; otherwise it cuts s
// at the last rune boundary at or before maxBytes and appends the truncation
// marker, so the result is always valid UTF-8.
func capBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}
