/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package branchreconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/workqueue"
)

func TestNew(t *testing.T) {
	cloneMeta := &clonemanager.Meta{}
	clientCache := &githubreconciler.ClientCache{}

	tests := []struct {
		name    string
		opts    []Option
		wantErr string
	}{
		{
			name:    "missing branchNamer",
			opts:    []Option{},
			wantErr: "branchNamer is required",
		},
		{
			name: "missing agentFunc",
			opts: []Option{
				WithBranchNamer(func(key string) (string, string, string, error) {
					return "owner", "repo", "branch", nil
				}),
			},
			wantErr: "agentFunc is required",
		},
		{
			name: "missing criteriaFunc",
			opts: []Option{
				WithBranchNamer(func(key string) (string, string, string, error) {
					return "owner", "repo", "branch", nil
				}),
				WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *BranchInfo) (string, error) {
					return "commit message", nil
				}),
			},
			wantErr: "criteriaFunc is required",
		},
		{
			name: "valid configuration",
			opts: []Option{
				WithBranchNamer(func(key string) (string, string, string, error) {
					return "owner", "repo", "branch", nil
				}),
				WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *BranchInfo) (string, error) {
					return "commit message", nil
				}),
				WithCriteriaFunc(func(ctx context.Context, info *BranchInfo) (bool, error) {
					return true, nil
				}),
			},
			wantErr: "",
		},
		{
			name: "with all options",
			opts: []Option{
				WithBranchNamer(func(key string) (string, string, string, error) {
					return "owner", "repo", "branch", nil
				}),
				WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *BranchInfo) (string, error) {
					return "commit message", nil
				}),
				WithCriteriaFunc(func(ctx context.Context, info *BranchInfo) (bool, error) {
					return true, nil
				}),
				WithOnSuccess(func(ctx context.Context, info *BranchInfo) error {
					return nil
				}),
				WithBaseBranch("develop"),
				WithMaxAttempts(5),
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := New(cloneMeta, clientCache, tt.opts...)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.Nil(t, rec)
			} else {
				require.NoError(t, err)
				require.NotNil(t, rec)
				require.NotNil(t, rec.branchNamer)
				require.NotNil(t, rec.agentFunc)
				require.NotNil(t, rec.criteriaFunc)
			}
		})
	}
}

func TestBranchNamer(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		namer      BranchNamer
		wantOwner  string
		wantRepo   string
		wantBranch string
		wantErr    bool
	}{
		{
			name: "simple key parsing",
			key:  "package/1.0.0",
			namer: func(key string) (string, string, string, error) {
				return "wolfi-dev", "os", "fix-bot/package-1.0.0", nil
			},
			wantOwner:  "wolfi-dev",
			wantRepo:   "os",
			wantBranch: "fix-bot/package-1.0.0",
			wantErr:    false,
		},
		{
			name: "invalid key format",
			key:  "invalid",
			namer: func(key string) (string, string, string, error) {
				return "", "", "", errors.New("invalid key format")
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, branch, err := tt.namer(tt.key)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.wantOwner, owner)
				require.Equal(t, tt.wantRepo, repo)
				require.Equal(t, tt.wantBranch, branch)
			}
		})
	}
}

func TestBranchInfo(t *testing.T) {
	info := &BranchInfo{
		Key:         "test-package/1.0.0",
		Owner:       "wolfi-dev",
		Repo:        "os",
		Branch:      "fix-bot/test-package-1.0.0",
		BaseBranch:  "main",
		HeadSHA:     "abc123",
		CommitCount: 3,
		BaseCommit:  "def456",
	}

	require.Equal(t, "test-package/1.0.0", info.Key)
	require.Equal(t, "wolfi-dev", info.Owner)
	require.Equal(t, "os", info.Repo)
	require.Equal(t, "fix-bot/test-package-1.0.0", info.Branch)
	require.Equal(t, "main", info.BaseBranch)
	require.Equal(t, "abc123", info.HeadSHA)
	require.Equal(t, 3, info.CommitCount)
	require.Equal(t, "def456", info.BaseCommit)
}

// TestProcessSubSecondRequeue checks that a requeue under a second reaches the
// wire as one second. Zero RequeueAfterSeconds tells the dispatcher the key is
// done, so truncating would drop the branch until its next resync. The
// requeue comes from minting the branch's GitHub token, which is the first
// step of a reconcile.
func TestProcessSubSecondRequeue(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantFloor bool
	}{{
		name: "requeue after",
		err:  workqueue.RequeueAfter(500 * time.Millisecond),
	}, {
		name:      "requeue not before",
		err:       workqueue.RequeueNotBefore(500 * time.Millisecond),
		wantFloor: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientCache := githubreconciler.NewClientCache(func(context.Context, string, string) (oauth2.TokenSource, error) {
				return nil, tt.err
			})
			rec, err := New(&clonemanager.Meta{}, clientCache,
				WithBranchNamer(func(string) (string, string, string, error) {
					return "owner", "repo", "branch", nil
				}),
				WithAgentFunc(func(context.Context, *git.Worktree, *BranchInfo) (string, error) {
					t.Error("agent ran: got = true, want = false")
					return "", nil
				}),
				WithCriteriaFunc(func(context.Context, *BranchInfo) (bool, error) {
					return true, nil
				}),
			)
			require.NoError(t, err)

			resp, err := rec.Process(t.Context(), &workqueue.ProcessRequest{Key: "owner/repo/branch"})
			require.NoError(t, err)
			if got := resp.GetRequeueAfterSeconds(); got != 1 {
				t.Errorf("RequeueAfterSeconds: got = %d, want = 1", got)
			}
			if got := resp.GetRequeueFloor(); got != tt.wantFloor {
				t.Errorf("RequeueFloor: got = %t, want = %t", got, tt.wantFloor)
			}
		})
	}
}
