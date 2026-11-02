/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/commentaudience"
	"github.com/chainguard-dev/clog"
	"github.com/shurcooL/githubv4"
)

// conversationIdentifierPrefix starts the identifier of a finding made from a
// comment in the pull request's conversation. It tells these findings apart
// from review-thread findings, whose identifiers are GraphQL node IDs, and from
// review-body findings.
const conversationIdentifierPrefix = "comment:"

// conversationIdentifier returns the finding identifier for the conversation
// comment with this ID.
func conversationIdentifier(id int64) string {
	return conversationIdentifierPrefix + strconv.FormatInt(id, 10)
}

// maxReplyQuoteBytes bounds the line of the comment quoted at the top of a reply.
const maxReplyQuoteBytes = 200

// replyMarkerPrefix and replyMarkerSuffix wrap the comment ID in the hidden tag
// at the top of a reply.
const (
	replyMarkerPrefix = "<!-- changemanager:reply-to:"
	replyMarkerSuffix = " -->"
)

// replyMarkerRE matches the hidden tag at the very start of a reply. The
// captured number is the ID of the comment the reply answers. The bot always
// writes its tag first, so a tag anywhere else in a comment is text someone
// else supplied, not a tag the bot wrote.
var replyMarkerRE = regexp.MustCompile(`\A` + regexp.QuoteMeta(replyMarkerPrefix) + `(\d+)` + regexp.QuoteMeta(replyMarkerSuffix))

// anyReplyMarkerRE matches a reply tag anywhere in a text. It is used to take
// tag-shaped text out of what a person or the agent wrote before that text goes
// into a reply.
var anyReplyMarkerRE = regexp.MustCompile(regexp.QuoteMeta(replyMarkerPrefix) + `\d+` + regexp.QuoteMeta(replyMarkerSuffix))

// replyMarker returns the hidden tag for a reply to the comment with this ID.
func replyMarker(commentID int64) string {
	return replyMarkerPrefix + strconv.FormatInt(commentID, 10) + replyMarkerSuffix
}

// gqlConversationComment is a comment in a pull request's conversation: not
// attached to a line of the diff, and not part of a review thread.
type gqlConversationComment struct {
	DatabaseId        int64
	Author            gqlActor
	AuthorAssociation string
	Body              string
	Url               string
	CreatedAt         string
	IsMinimized       bool
	// ViewerDidAuthor is true when the authenticated token's account wrote the
	// comment. The reply check relies on it because anyone can type a reply tag.
	ViewerDidAuthor bool `graphql:"viewerDidAuthor"`
}

// conversation is what queryConversationComments reads about a pull request.
type conversation struct {
	comments []gqlConversationComment
	// headCommitted is the date of the newest commit. It is the zero time when
	// the pull request has no commit.
	headCommitted time.Time
	// authorLogin is the login of the pull request's author, which is the bot
	// for a pull request the bot opened. It is empty when GitHub sends none.
	authorLogin string
}

// queryConversationComments reads the newest 100 conversation comments of a
// pull request, the date of its newest commit and its author's login. It runs
// only for a manager built with WithConversationComments, so the default pull
// request query is unchanged for every other bot. When the pull request has a
// commit but its date cannot be read, it returns no comments, so none can
// become a finding.
func queryConversationComments(ctx context.Context, gqlClient *graphqlclient.GraphQLClient, owner, repo string, number int) (conversation, error) {
	if number < 0 || number > math.MaxInt32 {
		return conversation{}, fmt.Errorf("pull request number %d outside int32", number)
	}
	var q struct {
		Repository struct {
			PullRequest *struct {
				Author struct {
					Login string
				}
				Comments struct {
					Nodes []gqlConversationComment
				} `graphql:"comments(last: 100)"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							CommittedDate string
						}
					}
				} `graphql:"commits(last: 1)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	if err := gqlClient.Query(ctx, "GetConversationComments", &q, map[string]any{
		"owner":  githubv4.String(owner),
		"repo":   githubv4.String(repo),
		"number": githubv4.Int(number),
	}); err != nil {
		return conversation{}, fmt.Errorf("querying conversation comments: %w", err)
	}
	pr := q.Repository.PullRequest
	if pr == nil {
		return conversation{}, nil
	}
	conv := conversation{comments: pr.Comments.Nodes, authorLogin: pr.Author.Login}
	if len(pr.Commits.Nodes) > 0 {
		// The zero time means no cutoff, so a date that cannot be read must not
		// fall back to it: every old comment would become a finding.
		committed, err := time.Parse(time.RFC3339, pr.Commits.Nodes[0].Commit.CommittedDate)
		if err != nil {
			clog.WarnContextf(ctx, "Skipping conversation comments: cannot read the head commit date of %s/%s#%d: %v", owner, repo, number, err)
			return conversation{authorLogin: pr.Author.Login}, nil
		}
		conv.headCommitted = committed
	}
	return conv, nil
}

// handledComments returns the IDs of the comments the bot has already replied
// to. Only a comment the bot wrote counts, and only a tag at its very start:
// the tag is plain text, so a tag in anyone else's comment proves nothing, and
// a tag further down a bot comment may be text the bot quoted or was given.
func handledComments(comments []gqlConversationComment) map[int64]struct{} {
	handled := make(map[int64]struct{})
	for _, c := range comments {
		if !c.ViewerDidAuthor {
			continue
		}
		if m := replyMarkerRE.FindStringSubmatch(c.Body); m != nil {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				handled[id] = struct{}{}
			}
		}
	}
	return handled
}

// collectConversationFindings turns the conversation comments that still need
// the bot into findings: written by someone trusted (see authorTrusted), not
// written by the bot, not minimized, not yet replied to, not mentioning anyone
// other than botLogin, the bot's login, and left after since, the time of the
// head commit.
//
// Like a review body, a comment left before the head commit already had its
// chance, so a new commit clears it. This also keeps old comments and chatter
// from turning into findings when a bot that already has pull requests turns
// the option on. A zero since means no cutoff; otherwise a comment whose time
// cannot be read is skipped. The bot's replies count as handled however old
// they are. A comment that mentions someone else is a talk between people, not
// a request to the bot; with an empty botLogin, any mention counts as one.
func collectConversationFindings(ctx context.Context, comments []gqlConversationComment, trustedAuthors map[string]struct{}, since time.Time, botLogin string) []callbacks.Finding {
	handled := handledComments(comments)
	findings := make([]callbacks.Finding, 0, len(comments))

	for _, c := range comments {
		if c.ViewerDidAuthor || c.IsMinimized {
			continue
		}
		if !createdAfter(c.CreatedAt, since) {
			clog.DebugContextf(ctx, "Skipping conversation comment left before the head commit id=%d", c.DatabaseId)
			continue
		}
		if !authorTrusted(c.AuthorAssociation, c.Author.Login, c.Author.Typename, trustedAuthors) {
			logUntrustedSkip(ctx, trustedAuthors, "Skipping untrusted conversation comment author=%s association=%s", displayReviewAuthor(c.Author.Login, c.Author.Typename), c.AuthorAssociation)
			continue
		}
		if _, ok := handled[c.DatabaseId]; ok {
			clog.DebugContextf(ctx, "Skipping conversation comment already replied to id=%d", c.DatabaseId)
			continue
		}
		if commentaudience.ForSomeoneElse(c.Body, botLogin) {
			clog.DebugContextf(ctx, "Skipping conversation comment that mentions someone else id=%d", c.DatabaseId)
			continue
		}
		findings = append(findings, callbacks.Finding{
			Kind:       callbacks.FindingKindReview,
			Identifier: conversationIdentifier(c.DatabaseId),
			Name:       "@" + displayReviewAuthor(c.Author.Login, c.Author.Typename),
			Details:    formatConversationDetails(c),
			DetailsURL: c.Url,
		})
	}
	return findings
}

// createdAfter reports whether createdAt, an RFC 3339 time, is after since. A
// zero since lets every comment through. A time that cannot be read does not
// pass, so a comment of unknown age cannot become a finding.
func createdAfter(createdAt string, since time.Time) bool {
	if since.IsZero() {
		return true
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	return err == nil && t.After(since)
}

// formatConversationDetails builds the text the agent reads for a conversation
// comment. The body is untrusted, so it goes inside the wrapper.
func formatConversationDetails(c gqlConversationComment) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pull request comment by @%s (%s)\n", displayReviewAuthor(c.Author.Login, c.Author.Typename), c.AuthorAssociation)
	fmt.Fprintf(&sb, "Posted: %s\n", c.CreatedAt)
	fmt.Fprintf(&sb, "\n%s\n", wrapQuoted("pr-comment", c.Body))
	return sb.String()
}

// conversationReplyBody builds the comment the bot posts to answer c. GitHub does
// not thread conversation comments, so the reply links the comment it answers
// and quotes its first line. The hidden tag at the top is how a later run knows
// the comment was handled. reply is the agent's text, already sanitized. Both the
// quote and the reply are text someone else supplied, so any reply tag in them
// is removed: the first line of the body is the only tag. Lines of the reply
// that start with a slash command are dropped too (see dropSlashCommandLines).
func conversationReplyBody(c gqlConversationComment, reply string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", replyMarker(c.DatabaseId))
	fmt.Fprintf(&sb, "In reply to %s\n", c.Url)
	if q := quoteFirstLine(c.Body); q != "" {
		fmt.Fprintf(&sb, "\n> %s\n", q)
	}
	fmt.Fprintf(&sb, "\n%s\n", dropSlashCommandLines(neutralizeReplyMarkers(reply)))
	return sb.String()
}

// neutralizeReplyMarkers replaces every reply tag in s, so text a person or the
// agent wrote cannot pass as a tag the bot wrote.
func neutralizeReplyMarkers(s string) string {
	return anyReplyMarkerRE.ReplaceAllString(s, "[tag removed]")
}

// isSlashCommandLine reports whether line starts with a slash command such as
// /retest. Other bots trim any Unicode space, a no-break space too, before they
// look for a command, so this does the same.
func isSlashCommandLine(line string) bool {
	rest, ok := strings.CutPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "/")
	if !ok || rest == "" {
		return false
	}
	c := rest[0]
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// dropSlashCommandLines removes the lines of s that start with a slash command.
// Other bots act on such a line in a conversation comment, so the reply must
// not carry one, whoever wrote the text. A slash later in a line, as in a path
// or a link, is kept.
func dropSlashCommandLines(s string) string {
	lines := strings.Split(s, "\n")
	return strings.Join(slices.DeleteFunc(lines, isSlashCommandLine), "\n")
}

// quoteFirstLine returns the first non-empty line of body, cleaned with the same
// rules as a reply and bounded, for quoting at the top of a reply. It returns
// nothing for a line that contains an @: GitHub would notify the person or team
// again when the bot quotes the mention.
func quoteFirstLine(body string) string {
	for line := range strings.SplitSeq(sanitizeReplyBody(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "@") {
			return ""
		}
		return neutralizeReplyMarkers(truncateOnRune(line, maxReplyQuoteBytes))
	}
	return ""
}
