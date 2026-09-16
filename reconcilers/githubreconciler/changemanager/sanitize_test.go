/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"strings"
	"testing"
)

// TestSanitizeReplyBody pins each reply-body rule: an @mention line is dropped,
// a markdown image and an off-site link are stripped while a github.com link
// stays, secret-shaped strings are redacted, control characters are removed,
// and runs of blank lines collapse.
func TestSanitizeReplyBody(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     string   // exact result when non-empty
		contains []string // substrings the result must keep
		omits    []string // substrings the result must not keep
	}{
		{
			name: "mention line dropped, prose mention kept",
			in:   "@some-reviewer ack\nplease see @user for context",
			want: "please see @user for context",
		},
		{
			name: "indented mention line dropped",
			in:   "  @some-reviewer resolve\nkeep this",
			want: "keep this",
		},
		{
			name:  "markdown image stripped",
			in:    "before ![pwn](https://evil.example/x.png) after",
			omits: []string{"evil.example", "![", "x.png"},
		},
		{
			name:     "github link kept, off-site link stripped",
			in:       "see https://github.com/chainguard-dev/mono/pull/1 not https://evil.example/pwn",
			contains: []string{"https://github.com/chainguard-dev/mono/pull/1"},
			omits:    []string{"evil.example"},
		},
		{
			name:     "github subdomain kept",
			in:       "api https://api.github.com/repos/x/y",
			contains: []string{"https://api.github.com/repos/x/y"},
		},
		{
			name:     "raw githubusercontent is off-site",
			in:       "blob https://raw.githubusercontent.com/x/y/main/z",
			omits:    []string{"raw.githubusercontent.com"},
			contains: []string{"blob"},
		},
		{
			name:     "github token redacted",
			in:       "token ghp_0123456789abcdefABCDEF here",
			contains: []string{secretRedaction},
			omits:    []string{"ghp_0123456789abcdefABCDEF"},
		},
		{
			name:     "oauth token redacted",
			in:       "gho_ZYXW9876543210abcd",
			contains: []string{secretRedaction},
			omits:    []string{"gho_ZYXW9876543210abcd"},
		},
		{
			name:     "fine-grained pat redacted",
			in:       "github_pat_11ABCDEFG0123456789_abcdefABCDEF end",
			contains: []string{secretRedaction},
			omits:    []string{"github_pat_11ABCDEFG"},
		},
		{
			name:     "aws access key redacted",
			in:       "key AKIAIOSFODNN7EXAMPLE done",
			contains: []string{secretRedaction},
			omits:    []string{"AKIAIOSFODNN7EXAMPLE"},
		},
		{
			name:     "pem private key block redacted",
			in:       "-----BEGIN RSA PRIVATE KEY-----\nMIIabc123\nlineTwo\n-----END RSA PRIVATE KEY-----",
			contains: []string{secretRedaction},
			omits:    []string{"MIIabc123", "BEGIN RSA PRIVATE KEY"},
		},
		{
			name:     "gitlab token redacted",
			in:       "ci glpat-abcdefgHIJKLmnop1234 end",
			contains: []string{secretRedaction},
			omits:    []string{"glpat-abcdefgHIJKLmnop1234"},
		},
		{
			name:     "gcp api key redacted",
			in:       "key AIzaSyA1234567890abcdefGHIJKLMNOPQRSTUV done",
			contains: []string{secretRedaction},
			omits:    []string{"AIzaSyA1234567890abcdefGHIJKLMNOPQRSTUV"},
		},
		{
			name:     "google oauth token redacted",
			in:       "tok ya29.a0ARrdaM-1234567890abcdefghij here",
			contains: []string{secretRedaction},
			omits:    []string{"ya29.a0ARrdaM-1234567890abcdefghij"},
		},
		{
			name:     "openai key redacted",
			in:       "sk-proj-abcdefghij1234567890ABCDEFGH end",
			contains: []string{secretRedaction},
			omits:    []string{"sk-proj-abcdefghij1234567890ABCDEFGH"},
		},
		{
			name:     "slack token redacted",
			in:       "hook xoxb-1234567890-ABCDEFabcdef done",
			contains: []string{secretRedaction},
			omits:    []string{"xoxb-1234567890-ABCDEFabcdef"},
		},
		{
			name:     "npm token redacted",
			in:       "npm_abcdefghij1234567890ABCDEFGHIJ1234567 end",
			contains: []string{secretRedaction},
			omits:    []string{"npm_abcdefghij1234567890ABCDEFGHIJ1234567"},
		},
		{
			name:     "pypi token redacted",
			in:       "pypi-AgEIcHlwaS5vcmcabcdef123456 end",
			contains: []string{secretRedaction},
			omits:    []string{"pypi-AgEIcHlwaS5vcmcabcdef123456"},
		},
		{
			name:     "huggingface token redacted",
			in:       "hf_abcdefghijABCDEFGHIJ1234567890 end",
			contains: []string{secretRedaction},
			omits:    []string{"hf_abcdefghijABCDEFGHIJ1234567890"},
		},
		{
			name:     "jwt redacted",
			in:       "bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcDEFghijKLMnop done",
			contains: []string{secretRedaction},
			omits:    []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
		},
		{
			name:  "control characters removed",
			in:    "line1\x07\x00more",
			want:  "line1more",
			omits: []string{"\x07", "\x00"},
		},
		{
			name: "blank runs collapse to one",
			in:   "a\n\n\n\nb",
			want: "a\n\nb",
		},
		{
			name: "leading and trailing blanks trimmed",
			in:   "\n\n\nkept\n\n\n",
			want: "kept",
		},
		{
			name: "crlf normalized",
			in:   "a\r\nb",
			want: "a\nb",
		},
		{
			name: "body of only a mention is emptied",
			in:   "@some-reviewer ack",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeReplyBody(tc.in)
			if tc.want != "" || (len(tc.contains) == 0 && len(tc.omits) == 0) {
				if got != tc.want {
					t.Errorf("sanitizeReplyBody(%q): got = %q, want = %q", tc.in, got, tc.want)
				}
			}
			for _, sub := range tc.contains {
				if !strings.Contains(got, sub) {
					t.Errorf("sanitizeReplyBody(%q) = %q, want it to contain %q", tc.in, got, sub)
				}
			}
			for _, sub := range tc.omits {
				if strings.Contains(got, sub) {
					t.Errorf("sanitizeReplyBody(%q) = %q, want it to omit %q", tc.in, got, sub)
				}
			}
		})
	}
}

// TestSanitizeReplyBodyCap bounds the result to the shared 4 KB quoted-body cap.
func TestSanitizeReplyBodyCap(t *testing.T) {
	got := sanitizeReplyBody(strings.Repeat("a", maxQuotedBodyBytes+2048))
	if len(got) > maxQuotedBodyBytes+len(truncationMarker) {
		t.Errorf("result length %d exceeds cap %d", len(got), maxQuotedBodyBytes+len(truncationMarker))
	}
	if !strings.Contains(got, truncationMarker) {
		t.Errorf("over-cap body missing truncation marker: %q", got)
	}
}

// TestSanitizeReplyBodyIdempotent proves a second pass is a no-op, so a body
// already stored and re-sanitized does not drift.
func TestSanitizeReplyBodyIdempotent(t *testing.T) {
	in := "@some-reviewer ack\nkeep https://github.com/a/b\n\n\ndrop https://evil.example/x\ntoken ghp_0123456789abcdef\n![i](https://evil.example/i.png)"
	once := sanitizeReplyBody(in)
	twice := sanitizeReplyBody(once)
	if once != twice {
		t.Errorf("sanitize not idempotent:\n once = %q\ntwice = %q", once, twice)
	}
}

// TestFindingRepliesOptionGating checks that the Reply callback, and therefore
// the reply tool, is present only when the manager enabled WithFindingReplies.
func TestFindingRepliesOptionGating(t *testing.T) {
	tests := []struct {
		name    string
		manager *CM[testData]
		want    bool
	}{
		{name: "enabled", manager: &CM[testData]{findingReplies: true}, want: true},
		{name: "disabled", manager: &CM[testData]{}, want: false},
		{name: "nil manager", manager: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session[testData]{manager: tc.manager}
			if got := s.FindingCallbacks().HasReply(); got != tc.want {
				t.Errorf("FindingCallbacks().HasReply(): got = %v, want = %v", got, tc.want)
			}
		})
	}
}
