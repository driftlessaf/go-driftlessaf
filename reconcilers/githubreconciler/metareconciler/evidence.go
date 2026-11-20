/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// EvidenceVersion is the only version of [Evidence] this package writes and
// reads. A record with any other version, including none, is not understood
// and reads as unknown (see [PRData.EvidenceRecord]).
const EvidenceVersion = 1

// Bounds on the dynamic fields [Evidence] persists.
const (
	evidenceSummaryMaxRunes   = 500
	evidenceVerdictMaxRunes   = 64
	evidenceMaxChecks         = 50
	evidenceCheckNameMaxRunes = 200
)

// EvidenceVerdictChanged is the verdict that declares the attempt proposed a
// change. A run that landed nothing never records it, whatever the agent
// declared: the commit guard may have dropped every edit.
const EvidenceVerdictChanged = "changed"

// EvidenceVerdictNoChange is the verdict recorded when the run landed nothing.
const EvidenceVerdictNoChange = "no_change"

// commitSHA matches a full commit SHA.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Evidence sources. They say who vouches for a piece of evidence, so a reader
// never mistakes what the agent said for what a check reported.
const (
	// EvidenceSourceAgentNote marks evidence the agent set itself: its
	// declared verdict and summary.
	EvidenceSourceAgentNote = "agent_note"

	// EvidenceSourceObservedCheck marks a check the reconciler observed on a
	// particular commit.
	EvidenceSourceObservedCheck = "observed_check"
)

// EvidenceDeclarer is an optional capability a [Result] may implement to
// declare the verdict and summary of its attempt. A result that does not
// implement it leaves no [Evidence] on the PR.
type EvidenceDeclarer interface {
	// DeclaredEvidence returns the agent's verdict and a summary of it. An
	// empty verdict declares nothing.
	DeclaredEvidence() (verdict, summary string)
}

// ObservedCheck is a check the reconciler observed failing on one commit.
type ObservedCheck struct {
	// Source is always [EvidenceSourceObservedCheck].
	Source string `json:"source"`

	// Name is the check's name.
	Name string `json:"name"`

	// HeadSHA is the commit the check ran against. It is the PR head the run
	// started from, never the head the run pushed.
	HeadSHA string `json:"head_sha"`

	// Conclusion is what the check reported.
	Conclusion string `json:"conclusion"`
}

// Evidence is the versioned record of a completed attempt, embedded in the
// PR's data. It binds the direction the attempt consumed to the exact PR head
// it evaluated.
//
// The record lives in PR body text that anyone with write access can edit, so
// it cannot authenticate anything on its own. A consumer must verify the live
// PR head against HeadSHA, the current intent against IssueBodyHash and
// SteeringID, and read the checks for a commit directly from GitHub; Checks
// only say what the producer saw when the attempt started. A check read for
// one head is never a claim about the next.
type Evidence struct {
	// Version is [EvidenceVersion].
	Version int `json:"version"`

	// IssueBodyHash and SteeringID are the direction the attempt consumed.
	// They equal the same-named fields of the PRData that carries the record.
	IssueBodyHash [32]byte `json:"issue_body_hash"`
	SteeringID    string   `json:"steering_id,omitempty"`

	// HeadSHA is the PR head the attempt evaluated: the pushed commit after a
	// push, the unchanged head after a run that changed nothing.
	HeadSHA string `json:"head_sha"`

	// InputHeadSHA is the PR head the attempt started from, empty when no PR
	// existed yet.
	InputHeadSHA string `json:"input_head_sha,omitempty"`

	// Verdict is what the agent declared, and Summary its bounded summary.
	// SummarySource is [EvidenceSourceAgentNote]: both come from the agent.
	Verdict       string `json:"verdict"`
	Summary       string `json:"summary,omitempty"`
	SummarySource string `json:"summary_source"`

	// Checks are the checks observed when the attempt started. Each names the
	// commit it ran against.
	Checks []ObservedCheck `json:"checks,omitempty"`
}

// ChecksForHead returns the observed checks that ran against HeadSHA. A check
// that ran against the input head is not one of them after a push.
func (e *Evidence) ChecksForHead() []ObservedCheck {
	var checks []ObservedCheck
	for _, c := range e.Checks {
		if c.HeadSHA == e.HeadSHA {
			checks = append(checks, c)
		}
	}
	return checks
}

// validate reports why e is incomplete or not of a known version.
func (e *Evidence) validate() error {
	switch {
	case e.Version != EvidenceVersion:
		return fmt.Errorf("unknown evidence version %d", e.Version)
	case e.IssueBodyHash == [32]byte{}:
		return errors.New("evidence has no issue body hash")
	case !commitSHA.MatchString(e.HeadSHA):
		return errors.New("evidence head SHA is missing or malformed")
	case e.InputHeadSHA != "" && !commitSHA.MatchString(e.InputHeadSHA):
		return errors.New("evidence input head SHA is malformed")
	case e.Verdict != EvidenceVerdictChanged && e.Verdict != EvidenceVerdictNoChange:
		return fmt.Errorf("evidence has no known verdict %q", truncateRunes(e.Verdict, evidenceVerdictMaxRunes))
	case e.Verdict == EvidenceVerdictChanged && e.HeadSHA == e.InputHeadSHA:
		return errors.New("evidence claims a change on an unchanged head")
	case e.SummarySource != EvidenceSourceAgentNote:
		return fmt.Errorf("unknown summary source %q", e.SummarySource)
	case len(e.Checks) > evidenceMaxChecks:
		return errors.New("evidence has too many checks")
	case len(e.Checks) > 0 && e.InputHeadSHA == "":
		return errors.New("evidence has checks but no input head")
	}
	for _, c := range e.Checks {
		switch {
		case c.Source != EvidenceSourceObservedCheck || c.Name == "" || c.Conclusion == "":
			return errors.New("incomplete observed check")
		case c.HeadSHA != e.InputHeadSHA:
			return errors.New("observed check ran against a head other than the input head")
		}
	}
	return nil
}

// EvidenceRecord returns the evidence the PR data carries. It returns nil and
// no error when there is none, as on data written before evidence existed:
// the attempt's head and verdict are unknown. It fails closed, returning an
// error, on a record of an unknown version, an incomplete one, or one whose
// direction is not the direction the data records.
func (d *PRData[Req]) EvidenceRecord() (*Evidence, error) {
	e := d.Evidence
	if e == nil {
		return nil, nil
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	if e.IssueBodyHash != d.IssueBodyHash || e.SteeringID != d.SteeringID {
		return nil, errors.New("evidence does not match the direction the PR data records")
	}
	return e, nil
}

// newEvidence builds the record of an attempt that evaluated head, started
// from inputHead, and consumed the direction in data. It returns nil when the
// result declares no verdict or head is unknown. Findings are the checks seen
// on inputHead; they are recorded against it and no other commit.
func newEvidence[Req any](data *PRData[Req], result any, head, inputHead string, findings []callbacks.Finding) *Evidence {
	declarer, ok := result.(EvidenceDeclarer)
	if !ok || head == "" {
		return nil
	}
	verdict, summary := declarer.DeclaredEvidence()
	if verdict == "" {
		return nil
	}
	e := &Evidence{
		Version:       EvidenceVersion,
		IssueBodyHash: data.IssueBodyHash,
		SteeringID:    data.SteeringID,
		HeadSHA:       head,
		InputHeadSHA:  inputHead,
		Verdict:       truncateRunes(verdict, evidenceVerdictMaxRunes),
		Summary:       truncateRunes(summary, evidenceSummaryMaxRunes),
		SummarySource: EvidenceSourceAgentNote,
	}
	if inputHead != "" {
		for _, f := range findings {
			if f.Kind != callbacks.FindingKindCICheck || len(e.Checks) == evidenceMaxChecks {
				continue
			}
			name := truncateRunes(cmp.Or(f.Name, f.Identifier), evidenceCheckNameMaxRunes)
			if name == "" {
				continue
			}
			e.Checks = append(e.Checks, ObservedCheck{
				Source:     EvidenceSourceObservedCheck,
				Name:       name,
				HeadSHA:    inputHead,
				Conclusion: "failure",
			})
		}
	}
	// A record that would not read back is not written.
	if e.validate() != nil {
		return nil
	}
	return e
}

// truncateRunes shortens s to at most n runes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
