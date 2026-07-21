/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubquestions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/go-github/v88/github"

	"chainguard.dev/driftlessaf/agents/suspend"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
)

// answerCommand is the comment prefix a human uses to answer the pending
// question: everything after it (same line and any following lines) is the
// raw answer text.
const answerCommand = "/answer"

// markerPrefix introduces the hidden HTML comment carrying the pending
// question as base64 JSON. Base64 keeps the marker safe no matter what the
// question text contains ("-->" in a prompt would otherwise terminate the
// HTML comment and corrupt the marker).
const markerPrefix = "<!-- askafriend:question:v1 "

// markerSuffix closes the hidden marker.
const markerSuffix = " -->"

// answeredMarker replaces the question marker once the question is consumed,
// so Pending stops surfacing it.
const answeredMarker = "<!-- askafriend:answered -->"

// Store is a suspend.QuestionStore that keeps the human transport on GitHub:
// Ask posts the question as a PR (or issue) comment carrying the pause nonce
// in a hidden marker, a collaborator replies with "/answer <text>", and
// Consume edits the question comment so it stops being pending. The
// conversation itself is the store — no state lives anywhere else, so a
// completely different process can pick the answer up.
//
// Keys must be in the githubreconciler PR/issue form "owner/repo#number".
//
// Unlike a CAS-backed store, GitHub comment edits are not atomic; the
// exactly-once guarantee of the lifecycle comes from the checkpoint Store's
// claim, not from this transport. Answer authorization is by the commenter's
// author association: OWNER, MEMBER, or COLLABORATOR.
type Store struct {
	backend commentsBackend
}

var _ suspend.QuestionStore = (*Store)(nil)

// New returns a Store that talks to GitHub through clients, which supplies a
// per-repository authenticated client (the same OctoSTS-backed cache the
// reconciler uses).
func New(clients *githubreconciler.ClientCache) *Store {
	return &Store{backend: &githubBackend{clients: clients}}
}

// parseKey splits an "owner/repo#number" key.
func parseKey(key string) (owner, repo string, number int, err error) {
	repoPart, numPart, ok := strings.Cut(key, "#")
	if !ok {
		return "", "", 0, fmt.Errorf("key %q is not in owner/repo#number form", key)
	}
	owner, repo, ok = strings.Cut(repoPart, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", 0, fmt.Errorf("key %q is not in owner/repo#number form", key)
	}
	number, err = strconv.Atoi(numPart)
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("key %q has an invalid number: %q", key, numPart)
	}
	return owner, repo, number, nil
}

// encodeMarker renders the hidden marker carrying q.
func encodeMarker(q suspend.Question) (string, error) {
	raw, err := json.Marshal(q)
	if err != nil {
		return "", fmt.Errorf("marshaling question: %w", err)
	}
	return markerPrefix + base64.StdEncoding.EncodeToString(raw) + markerSuffix, nil
}

// decodeMarker extracts the Question from a comment body, reporting ok=false
// when the body carries no (valid) marker.
func decodeMarker(body string) (suspend.Question, bool) {
	_, rest, found := strings.Cut(body, markerPrefix)
	if !found {
		return suspend.Question{}, false
	}
	b64, _, found := strings.Cut(rest, markerSuffix)
	if !found {
		return suspend.Question{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return suspend.Question{}, false
	}
	var q suspend.Question
	if err := json.Unmarshal(raw, &q); err != nil {
		return suspend.Question{}, false
	}
	return q, true
}

// questionBody renders the visible comment for q, marker included.
func questionBody(q suspend.Question) (string, error) {
	marker, err := encodeMarker(q)
	if err != nil {
		return "", err
	}
	quoted := "> " + strings.ReplaceAll(strings.TrimSpace(q.Prompt), "\n", "\n> ")
	return fmt.Sprintf(
		"⏸️ **The agent paused this run and needs a human decision**\n\n%s\n\nReply with `%s <your answer>` to resume (repository collaborators only).\n%s\n",
		quoted, answerCommand, marker), nil
}

// pendingIn finds the newest comment in comments carrying a question marker.
// Only app/bot-authored comments are considered: questions are posted by the
// reconciler's app identity, and honoring markers from human commenters would
// let anyone who can comment forge a newer "pending question" — superseding
// the real one (denying resume until its deadline) and putting attacker text
// into Pending. Nonce binding already prevents answer forgery; this closes
// the marker channel itself.
func pendingIn(comments []*github.IssueComment) (*github.IssueComment, suspend.Question, bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if comments[i].GetUser().GetType() != "Bot" {
			continue
		}
		if q, ok := decodeMarker(comments[i].GetBody()); ok {
			return comments[i], q, true
		}
	}
	return nil, suspend.Question{}, false
}

// pendingComment lists the conversation and finds the newest comment carrying
// a question marker.
func (s *Store) pendingComment(ctx context.Context, key string) (*github.IssueComment, suspend.Question, bool, error) {
	owner, repo, number, err := parseKey(key)
	if err != nil {
		return nil, suspend.Question{}, false, err
	}
	comments, err := s.backend.listComments(ctx, owner, repo, number)
	if err != nil {
		return nil, suspend.Question{}, false, fmt.Errorf("listing comments for %s: %w", key, err)
	}
	c, q, ok := pendingIn(comments)
	return c, q, ok, nil
}

// Ask posts q as a comment on the PR/issue named by key, superseding any
// previously pending question comment (its marker is retired so it can no
// longer be answered).
func (s *Store) Ask(ctx context.Context, key string, q suspend.Question) error {
	owner, repo, number, err := parseKey(key)
	if err != nil {
		return err
	}
	if prev, _, ok, err := s.pendingComment(ctx, key); err != nil {
		return err
	} else if ok {
		if err := s.retire(ctx, key, prev, "_(superseded by a newer question)_"); err != nil {
			return err
		}
	}
	body, err := questionBody(q)
	if err != nil {
		return err
	}
	if err := s.backend.createComment(ctx, owner, repo, number, body); err != nil {
		return fmt.Errorf("posting question comment on %s: %w", key, err)
	}
	return nil
}

// Pending returns the question carried by the newest question comment.
func (s *Store) Pending(ctx context.Context, key string) (suspend.Question, bool, error) {
	_, q, ok, err := s.pendingComment(ctx, key)
	return q, ok, err
}

// Answer scans the comments that arrived after the pending question comment
// for the first authorized "/answer <text>" reply, and returns it bound to
// the pending question's nonce. Replies from non-collaborators (or from apps
// and bots) are ignored. The conversation is listed exactly once — question
// and replies are located in the same page fetch, which matters because
// Answer runs on every poll wake of a parked run.
func (s *Store) Answer(ctx context.Context, key string) (suspend.Answer, bool, error) {
	owner, repo, number, err := parseKey(key)
	if err != nil {
		return suspend.Answer{}, false, err
	}
	comments, err := s.backend.listComments(ctx, owner, repo, number)
	if err != nil {
		return suspend.Answer{}, false, fmt.Errorf("listing comments for %s: %w", key, err)
	}
	qc, q, ok := pendingIn(comments)
	if !ok {
		return suspend.Answer{}, false, nil
	}
	for _, c := range comments {
		if c.GetID() <= qc.GetID() {
			continue // Posted before (or is) the question comment.
		}
		text, ok := answerText(c)
		if !ok {
			continue
		}
		return suspend.Answer{
			QuestionID: q.ID,
			Text:       text,
			AnsweredAt: c.GetCreatedAt().Time,
		}, true, nil
	}
	return suspend.Answer{}, false, nil
}

// answerText extracts the answer from an authorized "/answer" comment,
// reporting ok=false for anything else.
func answerText(c *github.IssueComment) (string, bool) {
	if c.GetUser().GetType() == "Bot" {
		return "", false
	}
	switch c.GetAuthorAssociation() {
	case "OWNER", "MEMBER", "COLLABORATOR":
	default:
		return "", false
	}
	body := strings.TrimSpace(c.GetBody())
	rest, found := strings.CutPrefix(body, answerCommand)
	if !found {
		return "", false
	}
	// Require a delimiter after the command so "/answered" is not an answer.
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '\n' {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// Consume retires the pending question comment when questionID matches its
// nonce, so a later Answer returns false until a new Ask. A nonce mismatch or
// an already-consumed question is a no-op.
func (s *Store) Consume(ctx context.Context, key, questionID string) error {
	qc, q, ok, err := s.pendingComment(ctx, key)
	if err != nil || !ok || q.ID != questionID {
		return err
	}
	return s.retire(ctx, key, qc, "✅ _Answered — the run is resuming._")
}

// retire rewrites a question comment so its marker no longer parses as
// pending, appending note to the visible text.
func (s *Store) retire(ctx context.Context, key string, c *github.IssueComment, note string) error {
	owner, repo, _, err := parseKey(key)
	if err != nil {
		return err
	}
	body := c.GetBody()
	if start := strings.Index(body, markerPrefix); start >= 0 {
		if end := strings.Index(body[start:], markerSuffix); end >= 0 {
			body = body[:start] + answeredMarker + body[start+end+len(markerSuffix):]
		}
	}
	body = strings.TrimRight(body, "\n") + "\n\n" + note + "\n"
	if err := s.backend.editComment(ctx, owner, repo, c.GetID(), body); err != nil {
		return fmt.Errorf("retiring question comment on %s: %w", key, err)
	}
	return nil
}
