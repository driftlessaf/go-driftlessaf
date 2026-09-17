/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-github/v88/github"
)

func TestIsDiffTooLarge(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "plain error",
			err:  errors.New("get PR diff: connection reset"),
			want: false,
		},
		{
			name: "406 without error entries",
			err:  &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotAcceptable}},
			want: true,
		},
		{
			name: "406 with too_large entry",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusNotAcceptable},
				Errors:   []github.Error{{Resource: "PullRequest", Field: "diff", Code: "too_large"}},
			},
			want: true,
		},
		{
			name: "too_large entry under another status",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
				Errors:   []github.Error{{Code: "too_large"}},
			},
			want: true,
		},
		{
			name: "404 is not too large",
			err:  &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}},
			want: false,
		},
		{
			name: "422 with an unrelated code",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
				Errors:   []github.Error{{Code: "custom"}},
			},
			want: false,
		},
		{
			name: "wrapped 406",
			err:  fmt.Errorf("get PR diff: %w", &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotAcceptable}}),
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDiffTooLarge(tc.err); got != tc.want {
				t.Errorf("isDiffTooLarge: got = %v, want = %v", got, tc.want)
			}
		})
	}
}

func TestOwnsPR(t *testing.T) {
	tests := []struct {
		name              string
		viewerDidAuthor   bool
		isCrossRepository bool
		want              bool
	}{
		{
			name:            "app authored, same repo",
			viewerDidAuthor: true,
			want:            true,
		},
		{
			name:              "app authored but from a fork",
			viewerDidAuthor:   true,
			isCrossRepository: true,
			want:              false,
		},
		{
			name:            "someone else authored on a same-repo branch borrowing the prefix",
			viewerDidAuthor: false,
			want:            false,
		},
		{
			name:              "someone else authored from a fork",
			viewerDidAuthor:   false,
			isCrossRepository: true,
			want:              false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownsPR(tc.viewerDidAuthor, tc.isCrossRepository); got != tc.want {
				t.Errorf("ownsPR(%v, %v): got = %v, want = %v", tc.viewerDidAuthor, tc.isCrossRepository, got, tc.want)
			}
		})
	}
}
