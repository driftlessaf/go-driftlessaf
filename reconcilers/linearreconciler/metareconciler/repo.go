/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
)

// resolveRepoTarget determines the GitHub repository for a Linear issue by
// reading the upstream bot's state attachment. Falls back to the configured
// RepoTargetResolver if no upstream state is found.
func (r *Reconciler[Req, Resp, CB]) resolveRepoTarget(ctx context.Context, issue *linearreconciler.Issue) (*RepoTarget, error) {
	title := r.upstreamPrefix + "_state"
	att := issue.FindAttachment(title)
	if att != nil && att.URL != "" {
		target, err := fetchRepoTarget(ctx, r.linearClient, att.URL)
		if err != nil {
			return nil, fmt.Errorf("reading upstream state %q: %w", title, err)
		}
		if target.Repo != "" {
			return target, nil
		}
	}

	if r.repoTargetResolver != nil {
		return r.repoTargetResolver(ctx, issue)
	}

	return nil, fmt.Errorf("no repo target found: no %q attachment and no fallback resolver configured", title)
}

// fetchRepoTarget downloads and deserializes a RepoTarget from an attachment URL.
func fetchRepoTarget(ctx context.Context, client *linearreconciler.Client, rawURL string) (*RepoTarget, error) {
	data, err := client.FetchAttachmentContent(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetching attachment content: %w", err)
	}

	// The upstream state may contain additional fields (metadata keys, bot-specific
	// state). We only extract the repo target fields.
	var target RepoTarget
	if err := json.Unmarshal(data, &target); err != nil {
		return nil, fmt.Errorf("unmarshaling repo target: %w", err)
	}
	return &target, nil
}

// splitOwnerRepo parses an "owner/repo" string into its components.
func splitOwnerRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo format %q: expected owner/repo", repo)
	}
	return parts[0], parts[1], nil
}
