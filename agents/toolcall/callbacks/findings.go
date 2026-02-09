/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import "context"

// FindingKind identifies the type of finding.
type FindingKind string

const (
	// FindingKindCICheck indicates a CI check failure.
	FindingKindCICheck FindingKind = "ciCheck"

	// FindingKindReview indicates a code review with feedback.
	FindingKindReview FindingKind = "review"
)

// Finding represents an issue that needs to be addressed.
// All details are populated at query time to avoid subsequent API calls.
type Finding struct {
	// Kind identifies the type of finding.
	Kind FindingKind `json:"kind" xml:"kind"`

	// Identifier is an opaque string that uniquely identifies this finding.
	Identifier string `json:"identifier" xml:"identifier"`

	// Details contains pre-fetched information about the finding.
	// For CI checks: name, status, conclusion, title, summary, text, detailsUrl
	Details string `json:"details" xml:"details"`

	// DetailsURL is the URL to the finding's details page (e.g., GitHub Actions job URL).
	// Used by GetLogs to fetch logs for the finding.
	DetailsURL string `json:"details_url" xml:"details_url"`
}

// FindingCallbacks provides callbacks for fetching finding details.
type FindingCallbacks struct {
	// Findings is the list of findings available in this context.
	// This allows extensions to access finding metadata (like DetailsURL)
	// without requiring a lookup callback.
	Findings []Finding

	// GetDetails retrieves detailed information about a finding.
	// Since details are pre-fetched in the GraphQL query, this just
	// looks up the finding by identifier and returns its Details field.
	GetDetails func(ctx context.Context, kind FindingKind, identifier string) (string, error)

	// GetLogs fetches logs for a finding (e.g., GitHub Actions job logs).
	// Returns the cleaned log content as a string.
	GetLogs func(ctx context.Context, kind FindingKind, identifier string) (string, error)
}

// HasGetDetails returns true if the GetDetails callback is available.
func (f FindingCallbacks) HasGetDetails() bool {
	return f.GetDetails != nil
}

// HasGetLogs returns true if the GetLogs callback is available.
func (f FindingCallbacks) HasGetLogs() bool {
	return f.GetLogs != nil
}

// GetFinding looks up a finding by kind and identifier.
// Returns nil if not found.
func (f FindingCallbacks) GetFinding(kind FindingKind, identifier string) *Finding {
	for i := range f.Findings {
		if f.Findings[i].Kind == kind && f.Findings[i].Identifier == identifier {
			return &f.Findings[i]
		}
	}
	return nil
}
