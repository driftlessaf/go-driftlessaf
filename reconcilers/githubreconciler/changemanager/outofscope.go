/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"path"
	"regexp"
	"slices"
	"strings"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/shurcooL/githubv4"
)

const (
	// MaxOutOfScopeRecords bounds the out-of-scope decisions kept in the PR
	// body. The newest records win.
	MaxOutOfScopeRecords = 8
	// MaxOutOfScopeEvidence bounds the evidence paths of one record.
	MaxOutOfScopeEvidence = 5
	// maxOutOfScopeConstraintBytes bounds a record's constraint text.
	maxOutOfScopeConstraintBytes = 512
)

// EvidenceDigest binds an out-of-scope decision to the content of one file it
// relied on. Digest is an opaque, stable identity of the file's content at the
// time of the decision (for example a git blob SHA).
type EvidenceDigest struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// OutOfScopeRecord is an accepted decision that a valid automated review
// finding asks for a change outside the issue's explicit constraints. It is
// persisted in the PR body (see Session.SetOutOfScopeRecords) so a later
// reconcile can recognize the same finding repeating.
type OutOfScopeRecord struct {
	// Fingerprint identifies the normalized finding; see ReviewBodyFingerprint.
	Fingerprint string `json:"fingerprint"`
	// Revision identifies the intent (issue title, body, and steering) the
	// decision was made under. A different revision never matches.
	Revision string `json:"revision"`
	// Constraint is the accepted constraint the remedy would have violated.
	Constraint string `json:"constraint"`
	// Evidence is the code the decision relied on, with its content identity.
	Evidence []EvidenceDigest `json:"evidence"`
}

var (
	reviewBodyHeaderRE = regexp.MustCompile(`^Review by @(\S+) \(`)
	reviewBodyQuotedRE = regexp.MustCompile(`(?s)<untrusted-content source="review-body">\n(.*)\n</untrusted-content>`)
)

// ReviewBodyFingerprint returns a stable fingerprint of a review-body finding:
// a digest of the reviewer's login and the exact review text. The submission time and commit are left out, so the
// same review repeated on a later push has the same fingerprint. automated
// reports whether the reviewer is a GitHub App (a "[bot]" login). ok is false
// for any finding that is not a review body of that shape, and for a body the
// renderer truncated.
func ReviewBodyFingerprint(f callbacks.Finding) (fingerprint string, automated, ok bool) {
	if f.Kind != callbacks.FindingKindReview || !strings.HasPrefix(f.Identifier, reviewBodyIdentifierPrefix) {
		return "", false, false
	}
	header := reviewBodyHeaderRE.FindStringSubmatch(f.Details)
	body := reviewBodyQuotedRE.FindStringSubmatch(f.Details)
	if header == nil || body == nil {
		return "", false, false
	}
	login := header[1]
	// A body cut at the display cap hides its tail, so a hash of the prefix
	// would match a later review that adds a new concern after the cut.
	// Refuse to fingerprint it; the finding then always reaches evaluation.
	if strings.HasSuffix(body[1], truncationMarker) {
		return "", false, false
	}
	text := body[1]
	if strings.TrimSpace(text) == "" || strings.Contains(text, "<neutralized-untrusted-content") {
		return "", false, false
	}
	sum := sha256.Sum256([]byte("exact-review-v1\x00" + login + "\n" + text))
	return hex.EncodeToString(sum[:]), strings.HasSuffix(login, botLoginSuffix), true
}

// NormalizeEvidencePath returns p cleaned as a repository-relative path, or
// false when it is empty, absolute, or escapes the repository.
func NormalizeEvidencePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || len(p) > maxInlinePathBytes || strings.HasPrefix(p, "/") || controlRunRE.MatchString(p) {
		return "", false
	}
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// OutOfScopeRecords returns the out-of-scope decisions recovered from the PR
// body plus any set in this session. The slice is the session's own; treat it
// as read-only.
func (s *Session[T]) OutOfScopeRecords() []OutOfScopeRecord {
	return s.meta.OutOfScope
}

// SetOutOfScopeRecords replaces the out-of-scope decisions the PR body keeps,
// bounding their number, constraint text, and evidence. Records without a
// fingerprint, revision, or evidence are dropped. The next Upsert or
// ReembedData persists them. Older bodies without the field parse as empty.
func (s *Session[T]) SetOutOfScopeRecords(records []OutOfScopeRecord) {
	kept := make([]OutOfScopeRecord, 0, len(records))
	for _, r := range records {
		if r.Fingerprint == "" || r.Revision == "" || len(r.Evidence) == 0 {
			continue
		}
		r.Constraint = truncateOnRune(r.Constraint, maxOutOfScopeConstraintBytes)
		if len(r.Evidence) > MaxOutOfScopeEvidence {
			r.Evidence = r.Evidence[:MaxOutOfScopeEvidence]
		}
		kept = append(kept, r)
	}
	if len(kept) > MaxOutOfScopeRecords {
		kept = kept[len(kept)-MaxOutOfScopeRecords:]
	}
	s.meta.OutOfScope = kept
}

// DropFindings removes every review-body finding for which drop reports true,
// so State and Findings no longer count it, and returns how many it removed.
// Only review bodies can be dropped: CI checks, review threads, and
// conversation comments always stay.
func (s *Session[T]) DropFindings(drop func(callbacks.Finding) bool) int {
	n := 0
	s.findings = slices.DeleteFunc(s.findings, func(f callbacks.Finding) bool {
		if _, automated, ok := ReviewBodyFingerprint(f); !ok || !automated || !drop(f) {
			return false
		}
		n++
		return true
	})
	return n
}

// BodyWrittenOnlyByViewer reports whether GitHub attributes the exact body, including
// every edit, to the authenticated App. A later bot rewrite cannot establish the
// origin of metadata copied from an earlier human edit.
func (s *Session[T]) BodyWrittenOnlyByViewer(ctx context.Context) bool {
	if s.prNumber < 1 || s.prNumber > math.MaxInt32 {
		return false
	}
	type actor struct {
		Bot      struct{ ID string } `graphql:"... on Bot"`
		Typename string              `graphql:"__typename"`
	}
	var q struct {
		Repository struct {
			PullRequest *struct {
				ViewerDidAuthor     bool
				Body                string
				HeadRefOid          string
				Author              *actor
				LastEditedAt        *string
				IncludesCreatedEdit bool
				UserContentEdits    *struct {
					TotalCount int
					PageInfo   struct{ HasNextPage bool }
					Nodes      []struct {
						DeletedAt *string
						Editor    *actor
					}
				} `graphql:"userContentEdits(first: 100)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	err := s.gqlClient.Query(ctx, "GetDispositionBodyProvenance", &q, map[string]any{
		"owner": githubv4.String(s.owner), "repo": githubv4.String(s.repo), "number": githubv4.Int(s.prNumber),
	})
	pr := q.Repository.PullRequest
	if err != nil || pr == nil || !pr.ViewerDidAuthor || pr.Body != s.prBody || pr.HeadRefOid != s.prHeadSHA {
		return false
	}
	if pr.Author == nil || pr.Author.Typename != "Bot" || pr.Author.Bot.ID == "" {
		return false
	}
	edits := pr.UserContentEdits
	if edits == nil || edits.PageInfo.HasNextPage || edits.TotalCount != len(edits.Nodes) {
		return false
	}
	if len(edits.Nodes) == 0 {
		return pr.LastEditedAt == nil
	}
	if !pr.IncludesCreatedEdit {
		return false
	}
	for _, edit := range edits.Nodes {
		if edit.DeletedAt != nil || edit.Editor == nil || edit.Editor.Typename != "Bot" || edit.Editor.Bot.ID != pr.Author.Bot.ID {
			return false
		}
	}
	return true
}
