/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Reply-body sanitization removes content a review reply must never carry back
// onto a pull request. The fixer composes the reply from model output derived
// from untrusted review text, so a body can carry a reviewer command, off-site
// content, or a leaked secret. Each rule below defuses one of those without
// rejecting the whole reply.

const secretRedaction = "[redacted]"

// secretPatterns lists distinctive secret shapes redacted from a reply body so
// a leaked credential is never echoed back onto a public PR. Each pattern is
// anchored on a vendor-specific prefix or a structural shape, so an ordinary
// reply is not mangled. The list is high-signal rather than exhaustive: a shape
// with no distinctive anchor (a bare AWS secret key, most hex API keys) is left
// to length bounds and human review.
var secretPatterns = []string{
	// PEM private-key block of any type, and a lone header.
	`(?s)-----BEGIN[ A-Z]*PRIVATE KEY-----.*?-----END[ A-Z]*PRIVATE KEY-----`,
	`-----BEGIN[ A-Z]*PRIVATE KEY-----`,
	// GitHub personal-access, OAuth, user, server, and refresh tokens, plus the
	// fine-grained PAT.
	`gh[pousr]_[A-Za-z0-9]{16,}`,
	`github_pat_[A-Za-z0-9_]{20,}`,
	// GitLab personal access token.
	`glpat-[A-Za-z0-9_-]{16,}`,
	// AWS access key id, long-term and temporary.
	`(?:AKIA|ASIA)[0-9A-Z]{12,}`,
	// Google API key and OAuth access token (GCP, Gemini).
	`AIza[0-9A-Za-z_-]{20,}`,
	`ya29\.[0-9A-Za-z_-]{20,}`,
	// OpenAI and Anthropic style keys.
	`sk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,}`,
	// Stripe secret and restricted keys.
	`(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`,
	// Slack bot, user, app, and refresh tokens.
	`xox[baprs]-[A-Za-z0-9-]{10,}`,
	// SendGrid API key.
	`SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`,
	// npm automation token.
	`npm_[A-Za-z0-9]{30,}`,
	// PyPI upload token.
	`pypi-[A-Za-z0-9_-]{16,}`,
	// Hugging Face access token.
	`hf_[A-Za-z0-9]{20,}`,
	// JWT: three base64url segments, the header segment starting with eyJ.
	`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,
}

var (
	// secretPatternRE matches any shape in secretPatterns.
	secretPatternRE = regexp.MustCompile(strings.Join(secretPatterns, "|"))

	// mentionLineRE matches a line that begins, after any leading whitespace,
	// with an @mention. A reviewer parses such a line as a disposition command,
	// so the whole line is dropped.
	mentionLineRE = regexp.MustCompile(`^[ \t]*@[A-Za-z0-9]`)

	// markdownImageRE matches an inline markdown image, which renders remote
	// content on the PR.
	markdownImageRE = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)

	// urlRE matches an http(s) URL up to the first delimiter, so a link embedded
	// in markdown or prose is found without swallowing its surrounding wrappers.
	urlRE = regexp.MustCompile(`https?://[^\s<>)\]]+`)
)

// sanitizeReplyBody strips content a review reply must not carry: it removes
// control characters, redacts secret-shaped strings, drops @mention lines,
// strips markdown images and off-site links, collapses runs of blank lines, and
// bounds the result to the same cap a quoted body uses. A body reduced to empty
// is returned empty; the caller refuses an empty reply before any request.
func sanitizeReplyBody(body string) string {
	// Normalize line endings so the line rules see one separator.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	// Remove control characters other than newline and tab.
	body = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, body)

	// Redact secret-shaped strings anywhere in the body, including multi-line
	// PEM blocks, before the line rules run.
	body = secretPatternRE.ReplaceAllString(body, secretRedaction)

	kept := make([]string, 0, strings.Count(body, "\n")+1)
	blank := true // start true so leading blank lines are dropped
	for line := range strings.SplitSeq(body, "\n") {
		if mentionLineRE.MatchString(line) {
			continue
		}
		line = markdownImageRE.ReplaceAllString(line, "")
		line = stripOffsiteURLs(line)
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if blank {
				continue // collapse a run of blank lines
			}
			blank = true
			kept = append(kept, "")
			continue
		}
		blank = false
		kept = append(kept, line)
	}
	// Drop a trailing blank line the collapse may have left.
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}

	return truncateOnRune(strings.Join(kept, "\n"), maxQuotedBodyBytes)
}

// stripOffsiteURLs removes every http(s) URL whose host is not github.com or a
// github.com subdomain, leaving on-site links intact. An unparseable URL is
// removed.
func stripOffsiteURLs(line string) string {
	return urlRE.ReplaceAllStringFunc(line, func(match string) string {
		u, err := url.Parse(match)
		if err != nil {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		if host == "github.com" || strings.HasSuffix(host, ".github.com") {
			return match
		}
		return ""
	})
}
