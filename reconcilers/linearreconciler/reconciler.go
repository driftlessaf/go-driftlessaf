/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
)

// ReconcilerFunc is the function signature for Linear issue reconcilers.
// It receives the fetched issue and the Linear client, and returns an error
// if reconciliation fails.
type ReconcilerFunc func(ctx context.Context, issue *Issue, client *Client) error

// Reconciler manages the reconciliation of Linear issues.
type Reconciler struct {
	workqueue.UnimplementedWorkqueueServiceServer

	reconcileFunc ReconcilerFunc
	client        *Client

	requiredLabel string
	teamFilter    string
}

// Option configures a Reconciler.
type Option func(*Reconciler)

// WithReconciler sets the reconciler function.
func WithReconciler(f ReconcilerFunc) Option {
	return func(r *Reconciler) {
		r.reconcileFunc = f
	}
}

// WithRequiredLabel configures a label gate: issues without this label are
// skipped (returns success without calling ReconcilerFunc).
func WithRequiredLabel(label string) Option {
	return func(r *Reconciler) {
		r.requiredLabel = label
	}
}

// WithTeamFilter configures a team filter: issues not belonging to this team
// key are skipped.
func WithTeamFilter(teamKey string) Option {
	return func(r *Reconciler) {
		r.teamFilter = teamKey
	}
}

// WithStatePrefix configures the prefix for state attachment titles.
// For example, WithStatePrefix("game") produces attachments titled "game_state".
// If not set, defaults to "reconciler" ("reconciler_state").
func WithStatePrefix(prefix string) Option {
	return func(r *Reconciler) {
		r.client.statePrefix = prefix
	}
}

// New creates a new Reconciler with the given client. It resolves the bot user
// identity by calling the Linear API.
func New(ctx context.Context, client *Client, opts ...Option) (*Reconciler, error) {
	r := &Reconciler{
		client: client,
	}

	for _, opt := range opts {
		opt(r)
	}

	// Resolve bot identity.
	viewer, err := client.GetViewer(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving bot identity: %w", err)
	}
	client.BotUserID = viewer.ID

	clog.FromContext(ctx).Infof("Linear reconciler bot user: %s (%s)", viewer.Name, viewer.ID)

	return r, nil
}

// Reconcile fetches the issue by key (UUID) and runs the ReconcilerFunc.
func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	if key == "" {
		return workqueue.NonRetriableError(fmt.Errorf("empty issue key"), "empty key")
	}

	issue, err := r.client.GetIssue(ctx, key)
	if err != nil {
		var rateLimitErr *RateLimitError
		if errors.As(err, &rateLimitErr) {
			return workqueue.RequeueAfter(addJitter(rateLimitErr.RetryAfter))
		}
		return fmt.Errorf("fetching issue: %w", err)
	}

	log := clog.FromContext(ctx).With("identifier", issue.Identifier, "title", issue.Title)

	if r.requiredLabel != "" && !issue.HasLabel(r.requiredLabel) {
		log.Infof("Issue missing required label %q, skipping", r.requiredLabel)
		return nil
	}

	if r.teamFilter != "" && issue.Team.Key != r.teamFilter {
		log.Infof("Issue team %q does not match filter %q, skipping", issue.Team.Key, r.teamFilter)
		return nil
	}

	if r.reconcileFunc == nil {
		return fmt.Errorf("no reconciler configured")
	}

	err = r.reconcileFunc(ctx, issue, r.client)
	if err != nil {
		var rateLimitErr *RateLimitError
		if errors.As(err, &rateLimitErr) {
			log.With("retry_after", rateLimitErr.RetryAfter).
				Warn("Rate limited, requeueing after retry period")
			return workqueue.RequeueAfter(addJitter(rateLimitErr.RetryAfter))
		}
	}
	return err
}

// Process implements the WorkqueueService.Process RPC.
func (r *Reconciler) Process(ctx context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	ctx = clog.WithValues(ctx, "key", req.Key)
	clog.InfoContextf(ctx, "Processing Linear issue: %s (priority: %d)", req.Key, req.Priority)

	err := r.Reconcile(ctx, req.Key)
	if err != nil {
		if delay, ok := workqueue.GetRequeueDelay(err); ok {
			clog.InfoContextf(ctx, "Reconciliation requested requeue after %v for key: %s", delay, req.Key)
			return &workqueue.ProcessResponse{
				RequeueAfterSeconds: int64(delay.Seconds()),
			}, nil
		}

		if queueKeys := workqueue.GetQueueKeys(err); len(queueKeys) > 0 {
			clog.InfoContextf(ctx, "Reconciliation requested queuing %d keys for key: %s", len(queueKeys), req.Key)
			resp := &workqueue.ProcessResponse{
				QueueKeys: make([]*workqueue.QueueKeyRequest, 0, len(queueKeys)),
			}
			for _, qk := range queueKeys {
				resp.QueueKeys = append(resp.QueueKeys, &workqueue.QueueKeyRequest{
					Key:          qk.Key,
					Priority:     qk.Priority,
					DelaySeconds: qk.DelaySeconds,
				})
			}
			return resp, nil
		}

		if details := workqueue.GetNonRetriableDetails(err); details != nil {
			clog.WarnContextf(ctx, "Reconciliation failed with non-retriable error for key %s: %v (reason: %s)", req.Key, err, details.Message)
			return &workqueue.ProcessResponse{}, nil
		}

		clog.ErrorContextf(ctx, "Reconciliation failed for key %s: %v", req.Key, err)
		return nil, err
	}

	clog.InfoContextf(ctx, "Successfully reconciled Linear issue: %s", req.Key)
	return &workqueue.ProcessResponse{}, nil
}

// addJitter adds random jitter to a duration to avoid thundering herd.
//
//nolint:gosec // Using weak random for jitter is fine, not cryptographic
func addJitter(d time.Duration) time.Duration {
	jitter := time.Duration(rand.Int63n(int64(d)))
	return d + jitter
}
