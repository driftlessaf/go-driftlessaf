/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// OutOfScopeDecision is the agent's decision that a valid automated review
// finding asks for a change outside the issue's explicit constraints.
type OutOfScopeDecision struct {
	// Identifier is the identifier of the finding the decision answers.
	Identifier string
	// Constraint is the accepted issue constraint the remedy would violate.
	// It must be a whole line, bullet, or sentence of the issue title or body
	// (after whitespace normalization, list marker and label removal) of at
	// least two words, or the decision is not retained.
	Constraint string
	// Evidence lists the repository paths whose content the decision relied on.
	Evidence []string
	// WholeReview is the agent's statement that every concern in the review
	// body is out of scope, with nothing in it fixed, refuted, or left
	// undecided. A decision without it is not retained, because the stored
	// fingerprint covers the whole body.
	WholeReview bool
}

// WithOutOfScopeDispositions retains an agent's out-of-scope decisions across
// pushes. After a run, decisions extracts them from the result; each one that
// answers an automated review body and quotes a constraint found verbatim in
// the issue is stored in the PR body with the intent revision (the issue title
// and body, steering included) and the git blob SHA of each evidence path. A
// later reconcile drops a review body from the findings only when its reviewer
// is a GitHub App, its text matches a stored decision exactly, the
// intent revision is unchanged, and every evidence path still has the recorded
// content. Anything else, a human reviewer, a different finding, new steering,
// a moved or unreadable evidence file, reaches the agent as before. CI checks,
// review threads, and conversation comments are never dropped. Off by default.
func WithOutOfScopeDispositions[Req promptbuilder.Bindable, Resp Result, CB any](decisions func(Resp) []OutOfScopeDecision) Option[Req, Resp, CB] {
	return func(r *Reconciler[Req, Resp, CB]) {
		r.outOfScope = decisions
	}
}

// intentRevision identifies the accepted scope a decision was made under: the
// issue title and body (steering included) and scope, which names any opt-in
// scope label the issue carries.
func intentRevision(issue *github.Issue, scope string) string {
	h := sha256.New()
	for _, part := range []string{issue.GetTitle(), issue.GetBody(), scope} {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// scopeRevision returns the intent revision of issue, which also covers the
// workflow-edit label because it widens what a run may commit.
func (r *Reconciler[Req, Resp, CB]) scopeRevision(issue *github.Issue) string {
	var scope string
	if r.workflowEditLabel != "" && hasLabel(issue, r.workflowEditLabel) {
		scope = "label:" + r.workflowEditLabel
	}
	return intentRevision(issue, scope)
}

// blobSHA returns the git blob SHA of the file at path on ref, or "" when the
// path is not a readable file.
func blobSHA(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, ref, path string) string {
	sha, missing, err := readBlobSHA(ctx, gh, res, ref, path)
	if err != nil || missing {
		return ""
	}
	return sha
}

// readBlobSHA returns the content identity for a file, whether the path is
// definitively missing, or an error when GitHub could not establish either.
func readBlobSHA(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, ref, path string) (string, bool, error) {
	file, _, response, err := gh.Repositories.GetContents(ctx, res.Owner, res.Repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return "", true, nil
		}
		return "", false, fmt.Errorf("read evidence file %q at %s: %w", path, ref, err)
	}
	if file == nil {
		return "", true, nil
	}
	if file.GetSHA() == "" {
		return "", false, fmt.Errorf("GitHub returned no blob SHA for evidence file %q at %s", path, ref)
	}
	return file.GetSHA(), false, nil
}

// minConstraintWords is the fewest words a constraint unit may have. A single
// word or fragment such as "the" proves nothing about what the issue forbids.
const minConstraintWords = 2

var (
	// constraintBullet matches a list marker at the start of a line.
	constraintBullet = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)
	// constraintLabel matches a short label such as "Constraint:".
	constraintLabel = regexp.MustCompile(`^(?i:constraints?):\s+`)
	// constraintLine matches a line labelled "Constraint:" outside a
	// constraints or steering section.
	constraintLine = regexp.MustCompile(`^(?:(?:[-*+]|\d+[.)])\s+)?(?i:constraints?):\s+\S`)
)

// constraintUnit normalizes one candidate constraint: whitespace collapsed,
// list marker and label removed, trailing punctuation trimmed. Case is kept.
func constraintUnit(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = constraintBullet.ReplaceAllString(s, "")
	s = constraintLabel.ReplaceAllString(s, "")
	return strings.TrimRight(s, ".;: ")
}

// IsExplicitConstraint reports whether the issue body names the whole constraint
// in a Constraints or Steering section, or labels it before the first heading.
func IsExplicitConstraint(body, constraint string) bool {
	c := constraintUnit(constraint)
	if len(strings.Fields(c)) < minConstraintWords {
		return false
	}
	source := []byte(body)
	doc := goldmark.DefaultParser().Parse(text.NewReader(source))
	inSection, sawHeading := false, false
	matches := func(block ast.Node) bool {
		if block.Kind() != ast.KindParagraph && block.Kind() != ast.KindTextBlock {
			return false
		}
		// Match source lines only when Markdown renders the entire block as text.
		for child := block.FirstChild(); child != nil; child = child.NextSibling() {
			if child.Kind() != ast.KindText {
				return false
			}
		}
		prose := strings.TrimSpace(string(block.Lines().Value(source)))
		if !inSection && (sawHeading || !constraintLine.MatchString(prose)) {
			return false
		}
		unit := constraintUnit(prose)
		if unit == c {
			return true
		}
		for sentence := range strings.SplitSeq(unit, ". ") {
			if constraintUnit(sentence) == c {
				return true
			}
		}
		return false
	}
	for block := doc.FirstChild(); block != nil; block = block.NextSibling() {
		switch block.Kind() {
		case ast.KindHeading:
			sawHeading = true
			heading := strings.TrimSpace(string(block.Lines().Value(source)))
			inSection = strings.EqualFold(heading, "Constraints") || strings.EqualFold(heading, "Steering")
		case ast.KindList:
			for item := block.FirstChild(); item != nil; item = item.NextSibling() {
				if first := item.FirstChild(); first != nil && matches(first) {
					return true
				}
			}
		case ast.KindParagraph:
			if matches(block) {
				return true
			}
		}
	}

	return false
}

// suppressDeclined drops automated review bodies that repeat a stored decision
// whose revision and evidence still hold. It reports how many findings were
// dropped and whether
// stale records were removed from the session for persistence.
func (r *Reconciler[Req, Resp, CB]) suppressDeclined(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, issue *github.Issue, session *changemanager.Session[PRData[Req]]) (int, bool) {
	records := session.OutOfScopeRecords()
	if r.outOfScope == nil || len(records) == 0 {
		return 0, false
	}
	findings := session.Findings()
	candidates := matchingOutOfScopeFindings(records, findings)
	if candidates == 0 {
		return 0, false
	}
	if !session.State().HasNoConflicts() || !session.BodyWrittenOnlyByViewer(ctx) {
		// Not suppressed, but do not bypass the per-head attempt guard.
		return 0, false
	}
	matching := make(map[string]struct{}, candidates)
	for _, finding := range findings {
		if fp, automated, ok := changemanager.ReviewBodyFingerprint(finding); ok && automated {
			matching[fp] = struct{}{}
		}
	}
	type blobRead struct {
		sha string
		err error
	}
	cache := map[string]blobRead{}
	suppressed := make(map[string]struct{}, candidates)
	validRecords := make([]changemanager.OutOfScopeRecord, 0, len(records))
	changed := false
	revision := r.scopeRevision(issue)
	for _, rec := range records {
		if _, candidate := matching[rec.Fingerprint]; !candidate {
			validRecords = append(validRecords, rec)
			continue
		}
		valid := rec.Revision == revision && len(rec.Evidence) > 0
		available := true
		if valid {
			for _, ev := range rec.Evidence {
				path, ok := changemanager.NormalizeEvidencePath(ev.Path)
				if !ok || path != ev.Path {
					valid = false
					break
				}
				read, cached := cache[path]
				if !cached {
					read.sha, _, read.err = readBlobSHA(ctx, gh, res, session.HeadSHA(), path)
					cache[path] = read
				}
				if read.err != nil {
					// Another path can still establish that this record is stale.
					available = false
					continue
				}
				if read.sha == "" || read.sha != ev.Digest {
					valid = false
					break
				}
			}
		}
		if valid {
			validRecords = append(validRecords, rec)
			if available {
				suppressed[rec.Fingerprint] = struct{}{}
			} else {
				delete(suppressed, rec.Fingerprint)
			}
		} else {
			changed = true
		}
	}
	if changed {
		session.SetOutOfScopeRecords(validRecords)
	}
	n := session.DropFindings(func(f callbacks.Finding) bool {
		fp, automated, ok := changemanager.ReviewBodyFingerprint(f)
		if !ok || !automated {
			return false
		}
		_, ok = suppressed[fp]
		return ok
	})
	if n > 0 {
		clog.InfoContext(ctx, "Ignoring automated review bodies that repeat an accepted out-of-scope decision", "dropped", n)
	}
	return n, changed
}

func matchingOutOfScopeFindings(records []changemanager.OutOfScopeRecord, findings []callbacks.Finding) int {
	n := 0
	for _, finding := range findings {
		fp, automated, ok := changemanager.ReviewBodyFingerprint(finding)
		if ok && automated && slices.ContainsFunc(records, func(record changemanager.OutOfScopeRecord) bool {
			return record.Fingerprint == fp
		}) {
			n++
		}
	}
	return n
}

// buildOutOfScopeRecords merges decisions into before. Records made under an
// older intent revision are pruned. A decision that names no automated review
// body in findings, quotes no constraint found in the issue, or lists an
// evidence path that cannot be read is ignored, so the finding is evaluated
// again next time.
func buildOutOfScopeRecords(before []changemanager.OutOfScopeRecord, decisions []OutOfScopeDecision, findings []callbacks.Finding, issue *github.Issue, revision string, blob func(path string) string) []changemanager.OutOfScopeRecord {
	records := slices.DeleteFunc(slices.Clone(before), func(rec changemanager.OutOfScopeRecord) bool {
		return rec.Revision != revision
	})
	digests := map[string]string{}
	cached := blob
	blob = func(p string) string {
		if sha, ok := digests[p]; ok {
			return sha
		}
		sha := cached(p)
		digests[p] = sha
		return sha
	}
	handled := map[string]struct{}{}
	for i, d := range decisions {
		if i >= changemanager.MaxOutOfScopeRecords {
			break
		}
		if _, dup := handled[d.Identifier]; dup {
			continue
		}
		handled[d.Identifier] = struct{}{}
		constraint := strings.TrimSpace(d.Constraint)
		if !d.WholeReview || !IsExplicitConstraint(issue.GetBody(), constraint) {
			continue
		}
		var fp string
		for _, f := range findings {
			if f.Identifier != d.Identifier {
				continue
			}
			if got, automated, ok := changemanager.ReviewBodyFingerprint(f); ok && automated {
				fp = got
			}
		}
		if fp == "" {
			continue
		}
		var evidence []changemanager.EvidenceDigest
		valid := true
		seen := map[string]struct{}{}
		for _, p := range d.Evidence {
			p, ok := changemanager.NormalizeEvidencePath(p)
			if !ok {
				valid = false
				break
			}
			if _, dup := seen[p]; dup {
				continue
			}
			if len(evidence) == changemanager.MaxOutOfScopeEvidence {
				valid = false
				break
			}
			seen[p] = struct{}{}
			sha := blob(p)
			if sha == "" {
				valid = false
				break
			}
			evidence = append(evidence, changemanager.EvidenceDigest{Path: p, Digest: sha})
		}
		if !valid || len(evidence) == 0 || len(evidence) > changemanager.MaxOutOfScopeEvidence {
			continue
		}
		records = slices.DeleteFunc(records, func(rec changemanager.OutOfScopeRecord) bool { return rec.Fingerprint == fp })
		records = append(records, changemanager.OutOfScopeRecord{Fingerprint: fp, Revision: revision, Constraint: constraint, Evidence: evidence})
	}
	return records
}

// recordOutOfScope stores the out-of-scope decisions in result on the session
// and reports whether the stored records changed. Nothing is recorded unless
// the run inspected the recorded PR head.
func (r *Reconciler[Req, Resp, CB]) recordOutOfScope(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, issue *github.Issue, session *changemanager.Session[PRData[Req]], result Resp, checkedOutPRHead bool, status gogit.Status) bool {
	if r.outOfScope == nil || session.PRNumber() == 0 || !checkedOutPRHead {
		return false
	}
	before := session.OutOfScopeRecords()
	// An evidence file the run changed may differ from the committed blob the
	// agent read, so its decision is not recorded.
	blob := func(p string) string {
		if st, ok := status[p]; ok && (st.Worktree != gogit.Unmodified || st.Staging != gogit.Unmodified) {
			return ""
		}
		return blobSHA(ctx, gh, res, session.HeadSHA(), p)
	}
	session.SetOutOfScopeRecords(buildOutOfScopeRecords(before, r.outOfScope(result), session.Findings(), issue, r.scopeRevision(issue), blob))
	return !slices.EqualFunc(before, session.OutOfScopeRecords(), func(a, b changemanager.OutOfScopeRecord) bool {
		return a.Fingerprint == b.Fingerprint && a.Revision == b.Revision && a.Constraint == b.Constraint && slices.Equal(a.Evidence, b.Evidence)
	})
}
