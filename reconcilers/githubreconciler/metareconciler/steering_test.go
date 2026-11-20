/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/google/go-github/v88/github"
)

const readyLabel = "test-bot/ready-for-review"

func TestNewestSteeringID(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{{
		name: "no section",
		body: "Please do the thing.",
	}, {
		name: "section without notes",
		body: "Please do the thing.\n\n## Steering\n",
	}, {
		name: "one note",
		body: "Please do the thing.\n\n## Steering\n\nuse a map\nSteering-ID: a1\n",
		want: "a1",
	}, {
		name: "newest note wins",
		body: "Do it.\n\n## Steering\n\nfirst\nSteering-ID: a1\n\nsecond\nSteering-ID: b2\n",
		want: "b2",
	}, {
		name: "quoted line is ignored",
		body: "Do it.\n\n## Steering\n\nSteering-ID: a1\n\n> Steering-ID: q9\n",
		want: "a1",
	}, {
		name: "indented line is ignored",
		body: "Do it.\n\n## Steering\n\n  Steering-ID: q9\n",
	}, {
		name: "line outside the section is ignored",
		body: "Steering-ID: x\n\n## Steering\n\nSteering-ID: a1\n\n## Other\n\nSteering-ID: y\n",
		want: "a1",
	}, {
		name: "CRLF line endings",
		body: "Do it.\r\n\r\n## Steering\r\n\r\nSteering-ID: a1\r\n",
		want: "a1",
	}, {
		name: "a hash line inside a note does not end the section",
		body: "## Steering\n\n#hashtag\nSteering-ID: a1\n",
		want: "a1",
	}, {
		name: "a deeper heading inside a note does not end the section",
		body: "## Steering\n\nnote\n### details\nSteering-ID: a1\n\nnote\n## details\nSteering-ID: b2\n",
		want: "a1",
	}, {
		name: "a level 3 heading in note one, note two still counts",
		body: "## Steering\n\nnote\n### details\nSteering-ID: a1\n\nnote\nSteering-ID: b2\n",
		want: "b2",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newestSteeringID(tt.body); got != tt.want {
				t.Errorf("newestSteeringID(): got = %q, want = %q", got, tt.want)
			}
		})
	}
}

func TestWithSteering(t *testing.T) {
	r := &Reconciler[*testRequest, *testResult, testCallbacks]{}
	WithSteering[*testRequest, *testResult, testCallbacks]()(r)
	if !r.steering {
		t.Error("reconciler.steering: got = false, want = true")
	}
}

func withSteering() Option[*testRequest, *publishResult, struct{}] {
	return WithSteering[*testRequest, *publishResult, struct{}]()
}

// steeringBody is an issue body with one note per id, oldest first.
func steeringBody(ids ...string) string {
	body := "Please do the thing."
	if len(ids) > 0 {
		body += "\n\n## Steering"
	}
	for _, id := range ids {
		body += "\n\nnote " + id + "\nSteering-ID: " + id
	}
	return body + "\n"
}

// steeringFake answers the requests of an open PR whose body, labels and
// writes the tests inspect.
type steeringFake struct {
	h       *issueHarness
	patches int
	removed []string
}

func (f *steeringFake) serve(w http.ResponseWriter, r *http.Request) bool {
	h := f.h
	const labels = "/repos/owner/repo/issues/456/labels"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/456":
		pr := &github.PullRequest{Number: new(456), Body: new(h.prText), Head: &github.PullRequestBranch{SHA: new(h.prTip())}}
		for _, l := range h.prLabels {
			pr.Labels = append(pr.Labels, &github.Label{Name: new(l)})
		}
		_ = json.NewEncoder(w).Encode(pr)
	case r.Method == http.MethodPatch && r.URL.Path == "/repos/owner/repo/pulls/456":
		var pr github.PullRequest
		_ = json.NewDecoder(r.Body).Decode(&pr)
		f.patches++
		h.prText = pr.GetBody()
		_ = json.NewEncoder(w).Encode(&github.PullRequest{Number: new(456)})
	case r.Method == http.MethodPost && r.URL.Path == labels:
		var added []string
		_ = json.NewDecoder(r.Body).Decode(&added)
		for _, l := range added {
			if !slices.Contains(h.prLabels, l) {
				h.prLabels = append(h.prLabels, l)
			}
		}
		w.Write([]byte(`[]`))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, labels+"/"):
		l := strings.TrimPrefix(r.URL.Path, labels+"/")
		f.removed = append(f.removed, l)
		h.prLabels = slices.DeleteFunc(h.prLabels, func(s string) bool { return s == l })
		w.Write([]byte(`[]`))
	default:
		return false
	}
	return true
}

// recorded returns the Steering-ID the open PR's body records.
func (h *issueHarness) recorded() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, rest, ok := strings.Cut(h.prText, `"steering_id": "`)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, `"`)
	return id
}

func (h *issueHarness) setIssueBody(body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.issue.Body = new(body)
}

func (h *issueHarness) hasPRLabel(label string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.prLabels, label)
}

// writes is an agent that changes content.txt to content.
func writes(content string) func(context.Context) error {
	return func(ctx context.Context) error {
		wt, ok := clonemanager.WorktreeFromContext(ctx)
		if !ok {
			return errors.New("missing leased worktree")
		}
		return os.WriteFile(filepath.Join(wt.Filesystem.Root(), "content.txt"), []byte(content+"\n"), 0o644)
	}
}

// steeringHarness opens a green PR for an issue with body, built by a real run
// under opts so that the PR's body carries what that run recorded. Counters
// start at zero afterwards.
func steeringHarness(t *testing.T, body string, opts ...Option[*testRequest, *publishResult, struct{}]) (*issueHarness, *steeringFake) {
	t.Helper()
	h := newIssueHarness(t)
	f := &steeringFake{h: h}
	h.extra = f.serve
	h.setIssueBody(body)
	h.reconcile(t, writes("one"), opts...)
	if h.prCreates != 1 || len(h.prBodies) != 1 {
		t.Fatalf("PRs created: got = %d, want = 1", h.prCreates)
	}
	h.openPR(t, "MERGEABLE", "[]")
	h.mu.Lock()
	h.prText = h.prBodies[0]
	h.prLabels = []string{requiredLabel, readyLabel}
	h.agentCalls = 0
	h.mu.Unlock()
	return h, f
}

func (h *issueHarness) wantRun(t *testing.T, name string, calls int) {
	t.Helper()
	if h.agentCalls != calls {
		t.Errorf("%s: agent calls: got = %d, want = %d", name, h.agentCalls, calls)
	}
}

func (f *steeringFake) wantWrites(t *testing.T, name string, patches int) {
	t.Helper()
	if f.patches != patches {
		t.Errorf("%s: PR body edits: got = %d, want = %d", name, f.patches, patches)
	}
}

func TestSteeringOptionOffIgnoresNotes(t *testing.T) {
	h, f := steeringHarness(t, steeringBody())
	h.setIssueBody(steeringBody("a"))
	h.reconcile(t, writes("two"))
	h.wantRun(t, "note with the option off", 0)
	f.wantWrites(t, "note with the option off", 0)
	if len(f.removed) != 0 {
		t.Errorf("labels removed: got = %q, want none", f.removed)
	}
	if got := h.recorded(); got != "" {
		t.Errorf("recorded steering ID: got = %q, want none", got)
	}
}

func TestSteeringGreenPRNewID(t *testing.T) {
	h, f := steeringHarness(t, steeringBody(), withSteering())
	if got := h.recorded(); got != "" {
		t.Fatalf("recorded steering ID at creation: got = %q, want none", got)
	}
	h.setIssueBody(steeringBody("a"))

	h.reconcile(t, writes("two"), withSteering())
	h.wantRun(t, "new note", 1)
	if !slices.Equal(f.removed, []string{readyLabel}) {
		t.Errorf("labels removed: got = %q, want = %q", f.removed, []string{readyLabel})
	}
	if got := h.recorded(); got != "a" {
		t.Errorf("recorded steering ID: got = %q, want = %q", got, "a")
	}
	f.wantWrites(t, "new note", 1)

	h.reconcile(t, writes("three"), withSteering())
	h.wantRun(t, "the next event", 1)
	f.wantWrites(t, "the next event", 1)
	if !h.hasPRLabel(readyLabel) {
		t.Error("ready-for-review after the green pass: got = absent, want = present")
	}
}

func TestSteeringNoOpRecordsOnce(t *testing.T) {
	h, f := steeringHarness(t, steeringBody(), withSteering())
	h.setIssueBody(steeringBody("a"))

	h.reconcile(t, noChange, withSteering())
	h.wantRun(t, "new note", 1)
	f.wantWrites(t, "new note", 1)
	if got := h.recorded(); got != "a" {
		t.Errorf("recorded steering ID: got = %q, want = %q", got, "a")
	}

	// The body changes again, with the same newest note.
	h.setIssueBody(steeringBody("a") + "\nA later edit.\n")
	h.reconcile(t, noChange, withSteering())
	// A label event on the PR.
	h.mu.Lock()
	h.prLabels = append(h.prLabels, "triage")
	h.mu.Unlock()
	h.reconcile(t, noChange, withSteering())
	h.wantRun(t, "later body and label events", 1)
	f.wantWrites(t, "later body and label events", 1)
	if !h.hasPRLabel(readyLabel) {
		t.Error("ready-for-review after the green pass: got = absent, want = present")
	}
}

func TestSteeringTwoNewIDsOneRun(t *testing.T) {
	h, f := steeringHarness(t, steeringBody(), withSteering())
	h.setIssueBody(steeringBody("a", "b"))

	h.reconcile(t, writes("two"), withSteering())
	h.reconcile(t, writes("three"), withSteering())
	h.wantRun(t, "two notes", 1)
	f.wantWrites(t, "two notes", 1)
	if got := h.recorded(); got != "b" {
		t.Errorf("recorded steering ID: got = %q, want = %q", got, "b")
	}
}

func TestSteeringFailedRunKeepsOldID(t *testing.T) {
	h, _ := steeringHarness(t, steeringBody("a"), withSteering())
	if got := h.recorded(); got != "a" {
		t.Fatalf("recorded steering ID at creation: got = %q, want = %q", got, "a")
	}
	h.setIssueBody(steeringBody("a", "b"))

	if err := h.reconcileErr(t, t.Context(), agentFails, withSteering()); err == nil {
		t.Fatal("failed run: got = nil, want the agent's error")
	}
	if got := h.recorded(); got != "a" {
		t.Errorf("recorded steering ID after a failed run: got = %q, want = %q", got, "a")
	}

	h.reconcile(t, writes("two"), withSteering())
	h.wantRun(t, "the retry", 2)
	if got := h.recorded(); got != "b" {
		t.Errorf("recorded steering ID after the retry: got = %q, want = %q", got, "b")
	}
}

func TestSteeringWithCIFinding(t *testing.T) {
	h, f := steeringHarness(t, steeringBody(), withSteering())
	h.setPR("MERGEABLE", failingCheck)
	h.setIssueBody(steeringBody("a"))

	h.reconcile(t, writes("two"), withSteering())
	h.wantRun(t, "finding and note", 1)
	f.wantWrites(t, "finding and note", 1)
	if got := h.recorded(); got != "a" {
		t.Errorf("recorded steering ID: got = %q, want = %q", got, "a")
	}

	h.setPR("MERGEABLE", "[]")
	h.reconcile(t, writes("three"), withSteering())
	h.wantRun(t, "the green pass", 1)
}

func TestSteeringPRCreatedFromNotedBody(t *testing.T) {
	h, f := steeringHarness(t, steeringBody("a"), withSteering())
	h.reconcile(t, writes("two"), withSteering())
	h.wantRun(t, "PR created from a noted body", 0)
	f.wantWrites(t, "PR created from a noted body", 0)
}

func TestSteeringQuotedIDIgnored(t *testing.T) {
	h, f := steeringHarness(t, steeringBody(), withSteering())
	h.setIssueBody("Please do the thing.\n\n## Steering\n\n> Steering-ID: q\n")
	h.reconcile(t, writes("two"), withSteering())
	h.wantRun(t, "quoted ID", 0)
	f.wantWrites(t, "quoted ID", 0)
}

func TestSteeringSkipAndTakeoverWin(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *issueHarness)
	}{{
		name: "skip label on the PR",
		setup: func(h *issueHarness) {
			h.prLabels = append(h.prLabels, "skip:test-bot")
		},
	}, {
		name: "PR assigned to a person",
		setup: func(h *issueHarness) {
			h.prAssignees = []string{"reviewer"}
		},
	}, {
		name: "skip label on the issue",
		setup: func(h *issueHarness) {
			h.issue.Labels = append(h.issue.Labels, &github.Label{Name: new("skip:test-bot")})
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, f := steeringHarness(t, steeringBody(), withSteering())
			h.mu.Lock()
			tt.setup(h)
			h.mu.Unlock()
			h.setIssueBody(steeringBody("a"))
			h.reconcile(t, writes("two"), withSteering())
			h.wantRun(t, tt.name, 0)
			f.wantWrites(t, tt.name, 0)
			if len(f.removed) != 0 {
				t.Errorf("labels removed: got = %q, want none", f.removed)
			}
		})
	}
}
