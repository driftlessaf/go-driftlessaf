/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import "testing"

func threadComment(login, typename, association, body, url string) gqlThreadComment {
	c := gqlThreadComment{AuthorAssociation: association, Body: body, Url: url}
	c.Author.Login = login
	c.Author.Typename = typename
	return c
}

func thread(id string, resolved bool, comments ...gqlThreadComment) gqlReviewThread {
	th := gqlReviewThread{Id: id, IsResolved: resolved, Path: "pkg/foo.go", Line: 42}
	th.Comments.Nodes = comments
	return th
}

// TestAuthorTrusted pins the login-normalization matching. A GitHub App author
// arrives from GraphQL bare with __typename "Bot"; from REST it is suffixed
// with "[bot]". An allowlist entry in either form matches either bot shape, and
// a non-Bot actor is never matched against the allowlist, in either the bare or
// the suffixed form, so a user named like an allowlisted App gains no trust.
func TestAuthorTrusted(t *testing.T) {
	const suffixed = "some-reviewer[bot]"
	const bare = "some-reviewer"

	suffixedAllow := map[string]struct{}{suffixed: {}}
	bareAllow := map[string]struct{}{bare: {}}

	tests := []struct {
		name        string
		association string
		login       string
		typename    string
		allowlist   map[string]struct{}
		want        bool
	}{
		{name: "trusted association passes with no allowlist", association: "MEMBER", login: "maintainer", typename: "User", allowlist: nil, want: true},
		{name: "owner association passes", association: "OWNER", login: "boss", typename: "User", allowlist: nil, want: true},
		{name: "graphql bot matches suffixed allowlist", association: "NONE", login: bare, typename: "Bot", allowlist: suffixedAllow, want: true},
		{name: "graphql bot matches bare allowlist", association: "NONE", login: bare, typename: "Bot", allowlist: bareAllow, want: true},
		{name: "rest suffixed login matches suffixed allowlist", association: "NONE", login: suffixed, typename: "Bot", allowlist: suffixedAllow, want: true},
		{name: "rest suffixed login matches bare allowlist", association: "NONE", login: suffixed, typename: "", allowlist: bareAllow, want: true},
		{name: "spoof: non-bot bare login cannot match suffixed allowlist", association: "NONE", login: bare, typename: "User", allowlist: suffixedAllow, want: false},
		{name: "spoof: non-bot user cannot match bare allowlist", association: "NONE", login: bare, typename: "User", allowlist: bareAllow, want: false},
		{name: "unlisted bot is not trusted", association: "NONE", login: "drive-by", typename: "Bot", allowlist: suffixedAllow, want: false},
		{name: "empty allowlist drops a NONE bot", association: "NONE", login: bare, typename: "Bot", allowlist: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := authorTrusted(tc.association, tc.login, tc.typename, tc.allowlist); got != tc.want {
				t.Errorf("authorTrusted(%q, %q, %q): got = %v, want = %v", tc.association, tc.login, tc.typename, got, tc.want)
			}
		})
	}
}

func TestDisplayReviewAuthor(t *testing.T) {
	tests := []struct {
		name     string
		login    string
		typename string
		want     string
	}{
		{name: "bot gets suffix restored", login: "some-reviewer", typename: "Bot", want: "some-reviewer[bot]"},
		{name: "already suffixed bot unchanged", login: "some-reviewer[bot]", typename: "Bot", want: "some-reviewer[bot]"},
		{name: "human login unchanged", login: "maintainer", typename: "User", want: "maintainer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayReviewAuthor(tc.login, tc.typename); got != tc.want {
				t.Errorf("displayReviewAuthor(%q, %q): got = %q, want = %q", tc.login, tc.typename, got, tc.want)
			}
		})
	}
}

func TestCollectThreadFindings(t *testing.T) {
	// The allowlist is written in the "[bot]" form the operator configures; the
	// bot's comments arrive from GraphQL bare with typename "Bot".
	const botAllow = "some-reviewer[bot]"
	const botLogin = "some-reviewer"
	allowBot := map[string]struct{}{botAllow: {}}

	tests := []struct {
		name           string
		thread         gqlReviewThread
		allowlist      map[string]struct{}
		wantCount      int
		wantDetailsURL string
	}{
		{
			name:      "trusted association",
			thread:    thread("t-assoc", false, threadComment("maintainer", "User", "MEMBER", "please fix", "https://gh/assoc")),
			allowlist: nil,
			wantCount: 1,
		},
		{
			name:      "graphql bot login matches suffixed allowlist",
			thread:    thread("t-bot", false, threadComment(botLogin, "Bot", "NONE", "unbounded read", "https://gh/bot")),
			allowlist: allowBot,
			wantCount: 1,
		},
		{
			// The REST shape: a suffixed login. Kept so both author shapes stay covered.
			name:      "rest suffixed bot login matches suffixed allowlist",
			thread:    thread("t-bot-rest", false, threadComment(botAllow, "Bot", "NONE", "unbounded read", "https://gh/bot-rest")),
			allowlist: allowBot,
			wantCount: 1,
		},
		{
			name:      "untrusted NONE login not in allowlist",
			thread:    thread("t-none", false, threadComment("drive-by", "User", "NONE", "nit", "https://gh/none")),
			allowlist: allowBot,
			wantCount: 0,
		},
		{
			name:      "human MEMBER unchanged with an allowlist set",
			thread:    thread("t-member", false, threadComment("maintainer", "User", "MEMBER", "please fix", "https://gh/member")),
			allowlist: allowBot,
			wantCount: 1,
		},
		{
			name:      "empty allowlist drops a NONE bot",
			thread:    thread("t-empty", false, threadComment(botLogin, "Bot", "NONE", "unbounded read", "https://gh/empty")),
			allowlist: nil,
			wantCount: 0,
		},
		{
			name: "only trusted comment is allowlisted bot",
			thread: thread("t-mixed", false,
				threadComment("drive-by", "User", "NONE", "ignore me", "https://gh/none"),
				threadComment(botLogin, "Bot", "NONE", "real finding", "https://gh/bot"),
			),
			allowlist:      allowBot,
			wantCount:      1,
			wantDetailsURL: "https://gh/bot",
		},
		{
			name:      "resolved thread skipped",
			thread:    thread("t-resolved", true, threadComment("maintainer", "User", "MEMBER", "already handled", "https://gh/resolved")),
			allowlist: nil,
			wantCount: 0,
		},
		{
			// A refuted finding the bot replied to and left open: settled, so it
			// is not resurfaced as a finding the fixer would answer again.
			name: "settled thread bot replied last yields no finding",
			thread: thread("t-settled", false,
				threadComment("maintainer", "User", "MEMBER", "please bound the read", "https://gh/settled"),
				viewerComment("this path is already bounded; leaving open"),
			),
			allowlist: nil,
			wantCount: 0,
		},
		{
			name: "trusted reply after the bot reopens the thread",
			thread: thread("t-reopened", false,
				threadComment("maintainer", "User", "MEMBER", "please bound the read", "https://gh/reopened"),
				viewerComment("this path is already bounded; leaving open"),
				threadComment("maintainer", "User", "MEMBER", "no, the second read is unbounded", "https://gh/reopened-2"),
			),
			allowlist:      nil,
			wantCount:      1,
			wantDetailsURL: "https://gh/reopened",
		},
		{
			// Resolved outranks awaiting: even a thread whose last relevant
			// comment is trusted is never a finding once resolved.
			name: "resolved thread never a finding even when awaiting",
			thread: thread("t-resolved-awaiting", true,
				threadComment("maintainer", "User", "MEMBER", "still open in spirit", "https://gh/resolved-awaiting"),
			),
			allowlist: nil,
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := gqlReviewThreadsConnection{Nodes: []gqlReviewThread{tc.thread}}
			got, _ := collectThreadFindings(t.Context(), conn, tc.allowlist)
			if len(got) != tc.wantCount {
				t.Fatalf("finding count: got = %d, want = %d (%+v)", len(got), tc.wantCount, got)
			}
			if tc.wantDetailsURL != "" && got[0].DetailsURL != tc.wantDetailsURL {
				t.Errorf("DetailsURL: got = %q, want = %q", got[0].DetailsURL, tc.wantDetailsURL)
			}
		})
	}
}

// viewerComment builds a comment authored by this bot (the authenticated
// viewer), as GraphQL reports it via viewerDidAuthor.
func viewerComment(body string) gqlThreadComment {
	return gqlThreadComment{ViewerDidAuthor: true, Body: body}
}

// TestThreadAwaitsReply pins the "who spoke last" semantic that gates commit
// budget renewal: a thread the bot answered last is settled, a trusted reply
// after the bot's reopens it, and untrusted comments neither settle nor reopen.
func TestThreadAwaitsReply(t *testing.T) {
	trusted := func(body string) gqlThreadComment {
		return threadComment("maintainer", "User", "MEMBER", body, "https://gh/member")
	}
	untrusted := func(body string) gqlThreadComment {
		return threadComment("drive-by", "User", "NONE", body, "https://gh/none")
	}

	tests := []struct {
		name     string
		comments []gqlThreadComment
		want     bool
	}{{
		name:     "bot answered last is settled",
		comments: []gqlThreadComment{trusted("please bound the read"), viewerComment("bounded it")},
		want:     false,
	}, {
		name:     "trusted reply after the bot reopens",
		comments: []gqlThreadComment{trusted("please bound the read"), viewerComment("bounded it"), trusted("still unbounded here")},
		want:     true,
	}, {
		name:     "only a trusted comment awaits the bot",
		comments: []gqlThreadComment{trusted("please bound the read")},
		want:     true,
	}, {
		name:     "only the bot spoke is settled",
		comments: []gqlThreadComment{viewerComment("nothing to do")},
		want:     false,
	}, {
		name:     "untrusted comment after the bot does not reopen",
		comments: []gqlThreadComment{trusted("please bound the read"), viewerComment("bounded it"), untrusted("me too")},
		want:     false,
	}, {
		name:     "untrusted comment alone does not await",
		comments: []gqlThreadComment{untrusted("random drive-by")},
		want:     false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := threadAwaitsReply(tc.comments, nil); got != tc.want {
				t.Errorf("threadAwaitsReply(): got = %v, want = %v", got, tc.want)
			}
		})
	}
}

func reviewBody(login, typename, association, body, oid, url string) gqlReviewBodyNode {
	r := gqlReviewBodyNode{DatabaseId: 7, AuthorAssociation: association, State: "COMMENTED", Body: body, Url: url}
	r.Author.Login = login
	r.Author.Typename = typename
	r.Commit.Oid = oid
	return r
}

func TestCollectReviewBodyFindings(t *testing.T) {
	const (
		botAllow = "some-reviewer[bot]"
		botLogin = "some-reviewer"
		head     = "headsha"
	)
	allowBot := map[string]struct{}{botAllow: {}}

	tests := []struct {
		name      string
		reviews   []gqlReviewBodyNode
		allowlist map[string]struct{}
		wantCount int
		wantName  string
	}{
		{
			name:      "trusted association",
			reviews:   []gqlReviewBodyNode{reviewBody("maintainer", "User", "MEMBER", "looks off", head, "https://gh/assoc")},
			allowlist: nil,
			wantCount: 1,
		},
		{
			name:      "graphql bot login matches suffixed allowlist",
			reviews:   []gqlReviewBodyNode{reviewBody(botLogin, "Bot", "NONE", "SSRF risk", head, "https://gh/bot")},
			allowlist: allowBot,
			wantCount: 1,
			wantName:  "@" + botAllow,
		},
		{
			name:      "untrusted NONE login not in allowlist",
			reviews:   []gqlReviewBodyNode{reviewBody("drive-by", "User", "NONE", "nit", head, "https://gh/none")},
			allowlist: allowBot,
			wantCount: 0,
		},
		{
			name:      "empty allowlist drops a NONE bot",
			reviews:   []gqlReviewBodyNode{reviewBody(botLogin, "Bot", "NONE", "SSRF risk", head, "https://gh/empty")},
			allowlist: nil,
			wantCount: 0,
		},
		{
			name: "only trusted review is allowlisted bot",
			reviews: []gqlReviewBodyNode{
				reviewBody("drive-by", "User", "NONE", "ignore me", head, "https://gh/none"),
				reviewBody(botLogin, "Bot", "NONE", "real finding", head, "https://gh/bot"),
			},
			allowlist: allowBot,
			wantCount: 1,
			wantName:  "@" + botAllow,
		},
		{
			name:      "stale commit skipped",
			reviews:   []gqlReviewBodyNode{reviewBody("maintainer", "User", "MEMBER", "on old commit", "othersha", "https://gh/stale")},
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
