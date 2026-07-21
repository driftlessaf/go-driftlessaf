/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubquestions

import (
	"errors"
	"testing"

	"github.com/google/go-github/v88/github"
)

// pagedComments builds n newest-first pages of perPage comments with globally
// descending IDs, mirroring how the GitHub API serves Direction: desc.
func pagedComments(n, perPage int) [][]*github.IssueComment {
	pages := make([][]*github.IssueComment, 0, n)
	id := int64(n * perPage)
	for range n {
		page := make([]*github.IssueComment, 0, perPage)
		for range perPage {
			page = append(page, &github.IssueComment{ID: github.Ptr(id)})
			id--
		}
		pages = append(pages, page)
	}
	return pages
}

func fetchFrom(pages [][]*github.IssueComment) func(page int) ([]*github.IssueComment, int, error) {
	return func(page int) ([]*github.IssueComment, int, error) {
		// Page 0 is the API default first page.
		idx := page
		if idx > 0 {
			idx-- // GitHub numbers pages from 1; NextPage values are 2, 3, ...
		}
		next := 0
		if idx+1 < len(pages) {
			next = idx + 2
		}
		return pages[idx], next, nil
	}
}

// TestCollectNewestFirstOrdering pins the contract every Store scan relies
// on: newest-first pages come back ASCENDING, so pendingIn's tail-first scan
// finds the newest marker. A dropped or inverted reversal fails this test.
func TestCollectNewestFirstOrdering(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pages     int
		wantLen   int
		truncated bool
	}{
		{name: "single page", pages: 1, wantLen: 3},
		{name: "multiple pages", pages: 3, wantLen: 9},
		{name: "at the bound", pages: maxCommentPages, wantLen: maxCommentPages * 3},
		{name: "beyond the bound truncates oldest", pages: maxCommentPages + 2, wantLen: maxCommentPages * 3, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated, err := collectNewestFirst(fetchFrom(pagedComments(tc.pages, 3)))
			if err != nil {
				t.Fatalf("collectNewestFirst: %v", err)
			}
			if truncated != tc.truncated {
				t.Errorf("truncated: got = %v, want = %v", truncated, tc.truncated)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len: got = %d, want = %d", len(got), tc.wantLen)
			}
			for i := 1; i < len(got); i++ {
				if got[i-1].GetID() >= got[i].GetID() {
					t.Fatalf("order at %d: %d >= %d, want strictly ascending IDs", i, got[i-1].GetID(), got[i].GetID())
				}
			}
			// Truncation must drop the OLDEST comments: the newest ID is
			// always retained.
			if got[len(got)-1].GetID() != int64(tc.pages*3) {
				t.Errorf("newest retained ID: got = %d, want = %d", got[len(got)-1].GetID(), tc.pages*3)
			}
		})
	}
}

func TestCollectNewestFirstError(t *testing.T) {
	boom := errors.New("boom")
	if _, _, err := collectNewestFirst(func(int) ([]*github.IssueComment, int, error) {
		return nil, 0, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("collectNewestFirst: got = %v, want the fetch error", err)
	}
}
