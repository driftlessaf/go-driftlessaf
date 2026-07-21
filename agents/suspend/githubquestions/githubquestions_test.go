/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubquestions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v88/github"

	"chainguard.dev/driftlessaf/agents/suspend"
)

// fakeBackend is an in-memory commentsBackend over one conversation,
// assigning ascending comment IDs the way GitHub does. lists counts
// listComments calls so tests can pin per-operation API cost.
type fakeBackend struct {
	comments []*github.IssueComment
	nextID   int64
	lists    int
}

var _ commentsBackend = (*fakeBackend)(nil)

func (f *fakeBackend) listComments(_ context.Context, _, _ string, _ int) ([]*github.IssueComment, error) {
	f.lists++
	return f.comments, nil
}

func (f *fakeBackend) createComment(_ context.Context, _, _ string, _ int, body string) error {
	f.nextID++
	f.comments = append(f.comments, &github.IssueComment{
		ID:                github.Ptr(f.nextID),
		Body:              github.Ptr(body),
		AuthorAssociation: github.Ptr("NONE"),
		User:              &github.User{Type: github.Ptr("Bot")},
		CreatedAt:         &github.Timestamp{Time: time.Date(2026, 7, 20, 0, 0, int(f.nextID), 0, time.UTC)},
	})
	return nil
}

func (f *fakeBackend) editComment(_ context.Context, _, _ string, commentID int64, body string) error {
	for _, c := range f.comments {
		if c.GetID() == commentID {
			c.Body = github.Ptr(body)
			return nil
		}
	}
	return fmt.Errorf("comment %d not found", commentID)
}

// reply appends a human comment with the given author association.
func (f *fakeBackend) reply(body, association string) {
	f.nextID++
	f.comments = append(f.comments, &github.IssueComment{
		ID:                github.Ptr(f.nextID),
		Body:              github.Ptr(body),
		AuthorAssociation: github.Ptr(association),
		User:              &github.User{Type: github.Ptr("User")},
		CreatedAt:         &github.Timestamp{Time: time.Date(2026, 7, 20, 0, 0, int(f.nextID), 0, time.UTC)},
	})
}

func newTestStore() (*Store, *fakeBackend) {
	fb := &fakeBackend{}
	return &Store{backend: fb}, fb
}

const testKey = "org/repo#42"

func question(id, prompt string) suspend.Question {
	return suspend.Question{ID: id, Key: testKey, Prompt: prompt, AskedAt: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)}
}

func TestLifecycle(t *testing.T) {
	s, fb := newTestStore()

	if _, ok, err := s.Pending(t.Context(), testKey); err != nil || ok {
		t.Fatalf("Pending on empty conversation: ok=%v err=%v, want false, nil", ok, err)
	}

	// A prompt containing the HTML-comment terminator must round-trip — this
	// is what the base64 marker exists for.
	prompt := "Use fix --> or feat?\nSecond line."
	if err := s.Ask(t.Context(), testKey, question("q1", prompt)); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	q, ok, err := s.Pending(t.Context(), testKey)
	if err != nil || !ok {
		t.Fatalf("Pending: ok=%v err=%v", ok, err)
	}
	if q.ID != "q1" || q.Prompt != prompt {
		t.Errorf("Pending: got = %+v, want ID=q1 and the round-tripped prompt", q)
	}
	if _, ok, err := s.Answer(t.Context(), testKey); err != nil || ok {
		t.Fatalf("Answer before any reply: ok=%v err=%v, want false, nil", ok, err)
	}

	fb.reply("/answer use feat, it is a new setting", "COLLABORATOR")
	ans, ok, err := s.Answer(t.Context(), testKey)
	if err != nil || !ok {
		t.Fatalf("Answer: ok=%v err=%v", ok, err)
	}
	if ans.QuestionID != "q1" || ans.Text != "use feat, it is a new setting" {
		t.Errorf("Answer: got = %+v, want QuestionID=q1 and the reply text", ans)
	}

	if err := s.Consume(t.Context(), testKey, "q1"); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if _, ok, err := s.Pending(t.Context(), testKey); err != nil || ok {
		t.Errorf("Pending after Consume: ok=%v err=%v, want false, nil", ok, err)
	}
	if !strings.Contains(fb.comments[0].GetBody(), "Answered") {
		t.Errorf("question comment after Consume: %q, want the answered note appended", fb.comments[0].GetBody())
	}
}

func TestAnswerAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name        string
		association string
		bot         bool
		want        bool
	}{
		{name: "collaborator accepted", association: "COLLABORATOR", want: true},
		{name: "owner accepted", association: "OWNER", want: true},
		{name: "member accepted", association: "MEMBER", want: true},
		{name: "drive-by rejected", association: "NONE", want: false},
		{name: "contributor rejected", association: "CONTRIBUTOR", want: false},
		{name: "bot rejected", association: "OWNER", bot: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fb := newTestStore()
			if err := s.Ask(t.Context(), testKey, question("q1", "which one?")); err != nil {
				t.Fatalf("Ask: %v", err)
			}
			fb.reply("/answer yes", tc.association)
			if tc.bot {
				fb.comments[len(fb.comments)-1].User.Type = github.Ptr("Bot")
			}
			if _, ok, err := s.Answer(t.Context(), testKey); err != nil || ok != tc.want {
				t.Errorf("Answer: ok=%v err=%v, want ok=%v", ok, err, tc.want)
			}
		})
	}
}

func TestAnswerCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{name: "same line", body: "/answer feat", want: "feat", ok: true},
		{name: "multiline", body: "/answer\nfeat — new setting", want: "feat — new setting", ok: true},
		{name: "surrounding whitespace", body: "  /answer   feat  ", want: "feat", ok: true},
		{name: "empty answer", body: "/answer", want: "", ok: true},
		{name: "prefix collision rejected", body: "/answered yes", ok: false},
		{name: "plain comment rejected", body: "looks good to me", ok: false},
		{name: "mid-comment command rejected", body: "you should reply /answer feat", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fb := newTestStore()
			if err := s.Ask(t.Context(), testKey, question("q1", "which one?")); err != nil {
				t.Fatalf("Ask: %v", err)
			}
			fb.reply(tc.body, "COLLABORATOR")
			ans, ok, err := s.Answer(t.Context(), testKey)
			if err != nil || ok != tc.ok {
				t.Fatalf("Answer: ok=%v err=%v, want ok=%v", ok, err, tc.ok)
			}
			if ok && ans.Text != tc.want {
				t.Errorf("Answer text: got = %q, want = %q", ans.Text, tc.want)
			}
		})
	}
}

// TestAnswerListsConversationOnce pins Answer's API cost: it runs on every
// poll wake of a parked run, so locating the question and its replies must
// share one conversation fetch.
func TestAnswerListsConversationOnce(t *testing.T) {
	s, fb := newTestStore()
	if err := s.Ask(t.Context(), testKey, question("q1", "which one?")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	fb.reply("/answer feat", "COLLABORATOR")

	fb.lists = 0
	if _, ok, err := s.Answer(t.Context(), testKey); err != nil || !ok {
		t.Fatalf("Answer: ok=%v err=%v", ok, err)
	}
	if fb.lists != 1 {
		t.Errorf("listComments calls during Answer: got = %d, want = 1", fb.lists)
	}
}

func TestRepliesBeforeQuestionIgnored(t *testing.T) {
	s, fb := newTestStore()
	fb.reply("/answer stale reply from an earlier pause", "COLLABORATOR")
	if err := s.Ask(t.Context(), testKey, question("q2", "which one?")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, ok, err := s.Answer(t.Context(), testKey); err != nil || ok {
		t.Errorf("Answer: ok=%v err=%v, want false — the reply predates the question", ok, err)
	}
}

func TestAskSupersedesPreviousQuestion(t *testing.T) {
	s, fb := newTestStore()
	if err := s.Ask(t.Context(), testKey, question("q1", "first?")); err != nil {
		t.Fatalf("Ask q1: %v", err)
	}
	fb.reply("/answer answer to q1", "COLLABORATOR")
	if err := s.Ask(t.Context(), testKey, question("q2", "second?")); err != nil {
		t.Fatalf("Ask q2: %v", err)
	}

	q, ok, err := s.Pending(t.Context(), testKey)
	if err != nil || !ok || q.ID != "q2" {
		t.Fatalf("Pending: q.ID=%q ok=%v err=%v, want q2", q.ID, ok, err)
	}
	// The q1 reply predates the q2 comment, so it must not surface for q2.
	if _, ok, err := s.Answer(t.Context(), testKey); err != nil || ok {
		t.Errorf("Answer: ok=%v err=%v, want false — the only reply answered the superseded q1", ok, err)
	}
	// And q1's marker is retired: only one pending marker exists.
	markers := 0
	for _, c := range fb.comments {
		if _, ok := decodeMarker(c.GetBody()); ok {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("pending markers in conversation: got = %d, want = 1", markers)
	}
}

// TestForgedMarkerFromHumanIgnored pins the question-channel authorization:
// a human commenter posting a syntactically valid marker must not supersede
// the app-posted question (which would deny the resume and spoof Pending) —
// markers are only honored in app/bot-authored comments.
func TestForgedMarkerFromHumanIgnored(t *testing.T) {
	s, fb := newTestStore()
	if err := s.Ask(t.Context(), testKey, question("q1", "the real question?")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	forged, err := questionBody(question("evil", "attacker-controlled prompt"))
	if err != nil {
		t.Fatalf("questionBody: %v", err)
	}
	fb.reply(forged, "COLLABORATOR") // Newest marker, but human-authored.

	q, ok, err := s.Pending(t.Context(), testKey)
	if err != nil || !ok || q.ID != "q1" {
		t.Fatalf("Pending: q.ID=%q ok=%v err=%v, want the app-posted q1", q.ID, ok, err)
	}
	// The real answer still binds to the real question despite the forgery.
	fb.reply("/answer proceed", "COLLABORATOR")
	ans, ok, err := s.Answer(t.Context(), testKey)
	if err != nil || !ok || ans.QuestionID != "q1" {
		t.Fatalf("Answer: got = %+v ok=%v err=%v, want the reply bound to q1", ans, ok, err)
	}
}

func TestConsumeNonceMismatchIsNoOp(t *testing.T) {
	s, _ := newTestStore()
	if err := s.Ask(t.Context(), testKey, question("q2", "which one?")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := s.Consume(t.Context(), testKey, "q1"); err != nil {
		t.Fatalf("Consume with stale nonce: %v, want nil no-op", err)
	}
	if _, ok, err := s.Pending(t.Context(), testKey); err != nil || !ok {
		t.Errorf("Pending after mismatched Consume: ok=%v err=%v, want the question intact", ok, err)
	}
	if err := s.Consume(t.Context(), "org/repo#7", "q1"); err != nil {
		t.Errorf("Consume on a conversation with no question: %v, want nil", err)
	}
}

func TestParseKey(t *testing.T) {
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{key: "org/repo#42", ok: true},
		{key: "org/repo", ok: false},
		{key: "orgrepo#42", ok: false},
		{key: "org/re/po#42", ok: false},
		{key: "org/repo#zero", ok: false},
		{key: "org/repo#-1", ok: false},
		{key: "", ok: false},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_, _, _, err := parseKey(tc.key)
			if (err == nil) != tc.ok {
				t.Errorf("parseKey(%q): err=%v, want ok=%v", tc.key, err, tc.ok)
			}
		})
	}
}
