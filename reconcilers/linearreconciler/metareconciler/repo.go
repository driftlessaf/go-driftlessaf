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
	"github.com/chainguard-dev/clog"
)

// resolveRepoTarget determines the GitHub repository for a Linear issue by
// reading the upstream bot's state attachment. Falls back to the configured
// RepoTargetResolver if no upstream state is found.
func (r *Reconciler[Req, Resp, CB, T, PT]) resolveRepoTarget(ctx context.Context, issue *linearreconciler.Issue) (*RepoTarget, error) {
	title := r.upstreamPrefix + "_state"
	trustedIDs := append([]string{r.linearClient.BotUserID}, r.upstreamBotUserIDs...)
	att := latestTrustedAttachment(ctx, issue, title, trustedIDs...)
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

// latestTrustedAttachment returns the newest attachment with the given title
// that was created by a trusted bot user, or nil. Attachments from any other
// creator (or with no creator) are ignored: any issue participant can add an
// attachment with the reserved title, and its repo field selects the GitHub
// repository this reconciler clones from and pushes to.
func latestTrustedAttachment(ctx context.Context, issue *linearreconciler.Issue, title string, trustedIDs ...string) *linearreconciler.Attachment {
	trusted := make(map[string]struct{}, len(trustedIDs))
	for _, id := range trustedIDs {
		if id != "" {
			trusted[id] = struct{}{}
		}
	}

	var latest *linearreconciler.Attachment
	for _, a := range issue.FindAttachmentsByTitle(title) {
		if a.Creator == nil {
			clog.WarnContextf(ctx, "Ignoring state attachment %q with no creator", a.ID)
			continue
		}
		if _, ok := trusted[a.Creator.ID]; !ok {
			clog.WarnContextf(ctx, "Ignoring state attachment %q from untrusted creator %q", a.ID, a.Creator.ID)
			continue
		}
		if latest == nil || a.CreatedAt.After(latest.CreatedAt) {
			latest = &a
		}
	}
	return latest
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
