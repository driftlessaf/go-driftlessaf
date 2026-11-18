/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import "strings"

const (
	// steeringHeading starts the issue body section that holds steering notes.
	steeringHeading = "## Steering"
	// steeringIDPrefix starts the line that carries a note's ID.
	steeringIDPrefix = "Steering-ID:"
)

// endsSteeringSection reports whether line is a markdown ATX heading of level
// 1 or 2, which ends the "## Steering" section. Deeper headings pasted into a
// note do not end it.
func endsSteeringSection(line string) bool {
	rest := strings.TrimLeft(line, "#")
	n := len(line) - len(rest)
	return n >= 1 && n <= 2 && strings.HasPrefix(rest, " ")
}

// newestSteeringID returns the ID of the newest note in the "## Steering"
// section of an issue body: the last line of the section that starts with
// "Steering-ID:". Quoted lines ("> Steering-ID:") and lines outside the section
// do not count. It returns "" when there is no such line.
func newestSteeringID(body string) string {
	var id string
	inSection := false
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.TrimRight(line, " \t") == steeringHeading:
			inSection = true
		case endsSteeringSection(line):
			inSection = false
		case inSection && strings.HasPrefix(line, steeringIDPrefix):
			id = strings.TrimSpace(strings.TrimPrefix(line, steeringIDPrefix))
		}
	}
	return id
}
