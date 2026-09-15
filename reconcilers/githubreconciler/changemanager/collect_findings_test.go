/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import "testing"

func threadComment(login, association, body, url string) gqlThreadComment {
	c := gqlThreadComment{AuthorAssociation: association, Body: body, Url: url}
	c.Author.Login = login
	return c
}

func thread(id string, resolved bool, comments ...gqlThreadComment) gqlReviewThread {
	th := gqlReviewThread{Id: id, IsResolved: resolved, Path: "pkg/foo.go", Line: 42}
	th.Comments.Nodes = comments
	return th
}

func TestCollectThreadFindings(t *testing.T) {
	const botLogin = "some-reviewer[bot]"
	allowBot := map[string]struct{}{botLogin: {}}

	tests := []struct {
		name           string
		thread         gqlReviewThread
		allowlist      map[string]struct{}
		wantCount      int
		wantDetailsURL string
	}{
		{
			name:      "trusted association",
			thread:    thread("t-assoc", false, threadComment("maintainer", "MEMBER", "please fix", "https://gh/assoc")),
			allowlist: nil,
			wantCount: 1,
		},
		{
			name:      "allowlisted bot login",
			thread:    thread("t-bot", false, threadComment(botLogin, "NONE", "unbounded read", "https://gh/bot")),
			allowlist: allowBot,
			wantCount: 1,
		},
		{
			name:      "untrusted NONE login not in allowlist",
			thread:    thread("t-none", false, threadComment("drive-by", "NONE", "nit", "https://gh/none")),
			allowlist: allowBot,
			wantCount: 0,
		},
		{
			name: "only trusted comment is allowlisted bot",
			thread: thread("t-mixed", false,
				threadComment("drive-by", "NONE", "ignore me", "https://gh/none"),
				threadComment(botLogin, "NONE", "real finding", "https://gh/bot"),
			),
			allowlist:      allowBot,
			wantCount:      1,
			wantDetailsURL: "https://gh/bot",
		},
		{
			name:      "resolved thread skipped",
			thread:    thread("t-resolved", true, threadComment("maintainer", "MEMBER", "already handled", "https://gh/resolved")),
			allowlist: nil,
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := gqlReviewThreadsConnection{Nodes: []gqlReviewThread{tc.thread}}
			got := collectThreadFindings(t.Context(), conn, tc.allowlist)
			if len(got) != tc.wantCount {
				t.Fatalf("finding count: got = %d, want = %d (%+v)", len(got), tc.wantCount, got)
			}
			if tc.wantDetailsURL != "" && got[0].DetailsURL != tc.wantDetailsURL {
				t.Errorf("DetailsURL: got = %q, want = %q", got[0].DetailsURL, tc.wantDetailsURL)
			}
		})
	}
}

func reviewBody(login, association, body, oid, url string) gqlReviewBodyNode {
	r := gqlReviewBodyNode{DatabaseId: 7, AuthorAssociation: association, State: "COMMENTED", Body: body, Url: url}
	r.Author.Login = login
	r.Commit.Oid = oid
	return r
}

func TestCollectReviewBodyFindings(t *testing.T) {
	const (
		botLogin = "some-reviewer[bot]"
		head     = "headsha"
	)
	allowBot := map[string]struct{}{botLogin: {}}

	tests := []struct {
		name      string
		reviews   []gqlReviewBodyNode
		allowlist map[string]struct{}
		wantCount int
		wantName  string
	}{
		{
			name:      "trusted association",
			reviews:   []gqlReviewBodyNode{reviewBody("maintainer", "MEMBER", "looks off", head, "https://gh/assoc")},
			allowlist: nil,
			wantCount: 1,
		},
		{
			name:      "allowlisted bot login",
			reviews:   []gqlReviewBodyNode{reviewBody(botLogin, "NONE", "SSRF risk", head, "https://gh/bot")},
			allowlist: allowBot,
			wantCount: 1,
		},
		{
			name:      "untrusted NONE login not in allowlist",
			reviews:   []gqlReviewBodyNode{reviewBody("drive-by", "NONE", "nit", head, "https://gh/none")},
			allowlist: allowBot,
			wantCount: 0,
		},
		{
			name: "only trusted review is allowlisted bot",
			reviews: []gqlReviewBodyNode{
				reviewBody("drive-by", "NONE", "ignore me", head, "https://gh/none"),
				reviewBody(botLogin, "NONE", "real finding", head, "https://gh/bot"),
			},
			allowlist: allowBot,
			wantCount: 1,
			wantName:  "@" + botLogin,
		},
		{
			name:      "stale commit skipped",
			reviews:   []gqlReviewBodyNode{reviewBody("maintainer", "MEMBER", "on old commit", "othersha", "https://gh/stale")},
			allowlist: nil,
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := gqlReviewBodiesConnection{Nodes: tc.reviews}
			got := collectReviewBodyFindings(t.Context(), head, conn, tc.allowlist)
			if len(got) != tc.wantCount {
				t.Fatalf("finding count: got = %d, want = %d (%+v)", len(got), tc.wantCount, got)
			}
			if tc.wantName != "" && got[0].Name != tc.wantName {
				t.Errorf("Name: got = %q, want = %q", got[0].Name, tc.wantName)
			}
		})
	}
}
