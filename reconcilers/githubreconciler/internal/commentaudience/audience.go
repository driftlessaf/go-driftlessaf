/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commentaudience

import (
	"regexp"
	"strings"
)

// mentionRE matches an @mention of a user, a bot or an org/team. The first
// group is the handle, without any "[bot]" suffix: for an org/team it is the
// org. The text before the @ must not be a letter, digit, '.', '_', '%', '+'
// or '-', so an email address such as a@example.com is not a mention.
var mentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9._%+\-])@([A-Za-z0-9][A-Za-z0-9-]{0,38})(?:\[bot\])?`)

// inlineCodeRE matches an inline code span on one line.
var inlineCodeRE = regexp.MustCompile("`[^`\n]*`")

// ForSomeoneElse reports whether body mentions anyone other than the bot whose
// login is botLogin. A mention of the bot itself does not count, and the
// match ignores case and a "[bot]" suffix on either side. Text in code and in
// quoted lines is not read, since a person does not mean it as a mention. With
// an empty botLogin any mention counts, so the bot stays out when unsure.
func ForSomeoneElse(body, botLogin string) bool {
	bot := strings.TrimSuffix(botLogin, "[bot]")
	for _, m := range mentionRE.FindAllStringSubmatch(stripNonMentionText(body), -1) {
		if bot == "" || !strings.EqualFold(m[1], bot) {
			return true
		}
	}
	return false
}

// stripNonMentionText removes the parts of body a person would not mean as a
// mention: fenced code blocks, quoted lines and inline code spans.
func stripNonMentionText(body string) string {
	var sb strings.Builder
	inFence := false
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(trimmed, ">") {
			continue
		}
		sb.WriteString(inlineCodeRE.ReplaceAllString(line, " "))
		sb.WriteByte('\n')
	}
	return sb.String()
}
