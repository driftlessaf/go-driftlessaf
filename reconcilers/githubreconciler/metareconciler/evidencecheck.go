/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"
)

// EvidenceCheckVersion is the only version of [EvidenceCheckDetails] this
// package writes and reads.
const EvidenceCheckVersion = 1

// EvidenceVerdictUnknown is the verdict of an attempt that cannot stand behind
// a verdict about the head it ran on: it declared none, declared one the
// checkout contradicts, or ran on a commit other than the PR head.
const EvidenceVerdictUnknown = "unknown"

// evidenceCheckConfig is the identity and publisher App of the evidence check.
type evidenceCheckConfig struct {
	identity string
	appID    int64
}

// EvidenceCheckDetails is the record the evidence check carries, one per
// commit. It is the latest attempt's result for HeadSHA, matched to the
// direction the attempt consumed. It holds no free text from the agent, so a
// rephrased summary is not a new record. The agent's summary stays in the PR
// data's [Evidence].
//
// A reader must ask GitHub for the check at the live PR head, scoped to the
// publisher App (see [ReadEvidenceCheck]), and must still read the CI checks
// of that head directly: Checks only say what the producer saw on
// InputHeadSHA when the attempt started.
type EvidenceCheckDetails struct {
	// Version is [EvidenceCheckVersion].
	Version int `json:"version"`

	IssueNumber int `json:"issue_number"`

	// IssueBodyHash and SteeringID are the direction the attempt consumed.
	IssueBodyHash [32]byte `json:"issue_body_hash"`
	SteeringID    string   `json:"steering_id,omitempty"`

	// HeadSHA is the commit the attempt evaluated and the check is attached to.
	HeadSHA string `json:"head_sha"`

	// InputHeadSHA is the PR head the attempt started from.
	InputHeadSHA string `json:"input_head_sha,omitempty"`

	// Verdict is [EvidenceVerdictChanged], [EvidenceVerdictNoChange] or
	// [EvidenceVerdictUnknown].
	Verdict string `json:"verdict"`

	// Checks are the checks observed on InputHeadSHA, never on HeadSHA unless
	// the two are the same commit.
	Checks []ObservedCheck `json:"checks,omitempty"`

	// FindingsDigest identifies the findings a no-op attempt saw. It lets a
	// later reconcile recognize an attempt already made for the same head,
	// intent and findings, so the bot's own check event does not run the
	// agent again. Empty when the attempt saw no findings or was a push.
	FindingsDigest string `json:"findings_digest,omitempty"`
}

// findingsDigest identifies a set of findings, "" for none. Review findings
// must be passed with their full text (see
// changemanager.Session.FindingsFullText); the clipped preview hides a tail
// that can change without changing the digest.
func findingsDigest(findings []callbacks.Finding, scope string) string {
	if len(findings) == 0 {
		return ""
	}
	keys := make([]string, 0, len(findings))
	for _, f := range findings {
		content := sha256.Sum256([]byte(f.Details))
		keys = append(keys, string(f.Kind)+"\x00"+f.Identifier+"\x00"+hex.EncodeToString(content[:]))
	}
	sort.Strings(keys)
	if scope != "" {
		keys = append(keys, "scope:"+scope)
	}
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}

// evidenceAttemptRecorded reports whether the evidence check already records
// an attempt for head, the intent in data and findings. Writing the check
// delivers a check_run event that queues the issue again, so without this a
// no-op with findings would run the agent once more for its own event, and
// again for as long as the outcome kept changing. A failed read is returned
// so the queue retries; a corrupt record reports false and the agent runs.
func (r *Reconciler[Req, Resp, CB]) evidenceAttemptRecorded(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, data *PRData[Req], head, digest string) (bool, error) {
	cfg := r.evidenceCheck
	if cfg == nil || digest == "" || !commitSHA.MatchString(head) {
		return false, nil
	}
	d, err := readEvidenceCheckRecord(ctx, gh, res.Owner, res.Repo, cfg.identity, cfg.appID, head)
	if err != nil {
		if isCheckPermissionError(err) {
			return false, nil
		}
		if _, ok := errors.AsType[*github.ErrorResponse](err); ok {
			return false, err
		}
		return false, nil
	}
	if d == nil {
		return false, nil
	}
	return d.IssueNumber == res.Number && d.HeadSHA == head && d.IssueBodyHash == data.IssueBodyHash && d.SteeringID == data.SteeringID && d.FindingsDigest == digest, nil
}

// validate reports why d is incomplete or not of a known version.
func (d *EvidenceCheckDetails) validate() error {
	if d.IssueNumber <= 0 {
		return errors.New("evidence check requires an issue number")
	}
	if d.Version != EvidenceCheckVersion {
		return fmt.Errorf("unknown evidence check version %d", d.Version)
	}
	if d.Verdict == EvidenceVerdictUnknown {
		switch {
		case d.IssueBodyHash == [32]byte{}:
			return errors.New("evidence check has no issue body hash")
		case !commitSHA.MatchString(d.HeadSHA):
			return errors.New("evidence check head SHA is missing or malformed")
		case d.InputHeadSHA != "" || len(d.Checks) > 0:
			return errors.New("unknown evidence check carries a claim")
		}
		return nil
	}
	return (&Evidence{
		Version:       EvidenceVersion,
		IssueBodyHash: d.IssueBodyHash,
		SteeringID:    d.SteeringID,
		HeadSHA:       d.HeadSHA,
		InputHeadSHA:  d.InputHeadSHA,
		Verdict:       d.Verdict,
		SummarySource: EvidenceSourceAgentNote,
		Checks:        d.Checks,
	}).validate()
}

// newEvidenceCheckDetails builds the record of an attempt that consumed the
// direction in data and evaluated head. A nil e records the attempt as
// unknown.
func newEvidenceCheckDetails[Req any](data *PRData[Req], head string, e *Evidence) EvidenceCheckDetails {
	d := EvidenceCheckDetails{
		Version:       EvidenceCheckVersion,
		IssueNumber:   data.IssueNumber,
		IssueBodyHash: data.IssueBodyHash,
		SteeringID:    data.SteeringID,
		HeadSHA:       head,
		Verdict:       EvidenceVerdictUnknown,
	}
	if e != nil && e.HeadSHA == head {
		d.InputHeadSHA, d.Verdict, d.Checks = e.InputHeadSHA, e.Verdict, e.Checks
	}
	return d
}

// pullRequestOf is the resource the evidence check is scoped to.
func pullRequestOf(owner, repo string) *githubreconciler.Resource {
	return &githubreconciler.Resource{Owner: owner, Repo: repo, Type: githubreconciler.ResourceTypePullRequest}
}

// publishEvidenceCheck writes a neutral check for the attempted head. A failed
// first write leaves no evidence. Once a record exists, a failed update must
// reach the caller so reconciliation retries replacing the stale verdict.
func (r *Reconciler[Req, Resp, CB]) publishEvidenceCheck(ctx context.Context, gh *github.Client, res *githubreconciler.Resource, d EvidenceCheckDetails) error {
	cfg := r.evidenceCheck
	if cfg == nil || d.validate() != nil {
		return nil
	}
	sm, err := statusmanager.NewStatusManager[EvidenceCheckDetails](ctx, cfg.identity,
		statusmanager.WithPublisherAppID(cfg.appID),
		statusmanager.WithSkipUnchangedUpdates(),
		statusmanager.WithoutDetailsURL())
	if err != nil {
		clog.WarnContext(ctx, "Failed to create the evidence check manager", "error", err)
		return nil
	}
	session := sm.NewSession(gh, pullRequestOf(res.Owner, res.Repo), d.HeadSHA)
	existing, err := session.ObservedState(ctx)
	if err != nil {
		if isCheckPermissionError(err) {
			clog.WarnContext(ctx, "Cannot read the evidence check", "error", err)
			return nil
		}
		return fmt.Errorf("read evidence check: %w", err)
	}
	err = session.SetActualState(ctx, cfg.identity+": "+d.Verdict, &statusmanager.Status[EvidenceCheckDetails]{
		Status:     "completed",
		Conclusion: "neutral",
		Details:    d,
	})
	if err != nil {
		if existing == nil {
			clog.WarnContext(ctx, "Failed to publish the evidence check", "error", err)
			return nil
		}
		return fmt.Errorf("supersede evidence check: %w", err)
	}
	return nil
}

// isCheckPermissionError reports whether err shows the App cannot use checks.
func isCheckPermissionError(err error) bool {
	if ge, ok := errors.AsType[*github.ErrorResponse](err); ok && ge.Response != nil {
		return ge.Response.StatusCode == http.StatusForbidden || ge.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// ReadEvidenceCheck returns the latest attempt's record for the commit headSHA
// that a consumer holds as the live PR head, or nil when the attempt is
// unknown. It is the authoritative read of the latest attempt: the PR data's
// [Evidence] only describes the last push and can be older or edited.
//
// It reads only a neutral, completed check published by the App appID under
// identity, so a same-named check from another App is not seen, and it returns
// nil when the record is for another commit or another direction than
// headSHA, issueBodyHash and steeringID, which the consumer takes from the
// live PR head and the current issue. An error means the check could not be
// read or is corrupt; a reader treats it as unknown too. A zero appID reads
// nothing.
func ReadEvidenceCheck(ctx context.Context, gh *github.Client, owner, repo, identity string, appID int64, issueNumber int, headSHA string, issueBodyHash [32]byte, steeringID string) (*EvidenceCheckDetails, error) {
	d, err := readEvidenceCheckRecord(ctx, gh, owner, repo, identity, appID, headSHA)
	if err != nil || d == nil {
		return nil, err
	}
	if d.IssueNumber != issueNumber || d.IssueBodyHash != issueBodyHash || d.SteeringID != steeringID || d.Verdict == EvidenceVerdictUnknown {
		return nil, nil
	}
	return d, nil
}

// readEvidenceCheckRecord returns the validated record the App's check holds
// for headSHA, an unknown one included, or nil when there is none.
func readEvidenceCheckRecord(ctx context.Context, gh *github.Client, owner, repo, identity string, appID int64, headSHA string) (*EvidenceCheckDetails, error) {
	if appID == 0 || identity == "" || !commitSHA.MatchString(headSHA) {
		return nil, nil
	}
	sm, err := statusmanager.NewReadOnlyStatusManager[EvidenceCheckDetails](ctx, identity,
		statusmanager.WithPublisherAppID(appID),
		statusmanager.WithoutDetailsURL())
	if err != nil {
		return nil, fmt.Errorf("create evidence check reader: %w", err)
	}
	status, err := sm.ObservedStateAtSHA(ctx, gh, pullRequestOf(owner, repo), headSHA)
	if err != nil {
		return nil, fmt.Errorf("read evidence check: %w", err)
	}
	if status == nil || status.Status != "completed" || status.Conclusion != "neutral" || status.ObservedGeneration != headSHA {
		return nil, nil
	}
	d := status.Details
	if err := d.validate(); err != nil {
		return nil, err
	}
	if d.HeadSHA != headSHA {
		return nil, nil
	}
	return &d, nil
}
