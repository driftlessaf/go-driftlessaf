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
	"slices"
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

	requiredLabels []string
	// labelPredicates are evaluated after requiredLabels. Each receives the
	// issue and a slice of its label names (precomputed once per Reconcile).
	labelPredicates []func(*Issue, []string) bool
	teamFilter      string
}

// Option configures a Reconciler.
type Option func(*Reconciler)

// WithReconciler sets the reconciler function.
func WithReconciler(f ReconcilerFunc) Option {
	return func(r *Reconciler) {
		r.reconcileFunc = f
	}
}

// WithRequiredLabel configures an OR-semantics label gate: issues without
// any of the specified labels are skipped. Repeated calls and variadic args
// accumulate into the same OR set — an issue is accepted if it has at least
// one of the listed labels.
//
// For AND or NOT semantics, use WithAllRequiredLabels, WithoutLabel, or
// WithLabelPredicate. Multiple gates compose: the OR set must match AND every
// predicate must return true.
func WithRequiredLabel(labels ...string) Option {
	return func(r *Reconciler) {
		r.requiredLabels = append(r.requiredLabels, labels...)
	}
}

// WithAllRequiredLabels configures an AND-semantics label gate: the issue must
// carry every listed label (case-insensitive, matching Issue.HasLabel) or it
// is skipped. Multiple WithAllRequiredLabels options compose by AND.
//
// Calling with zero labels is a no-op (returns no predicate) so a forgotten
// argument list doesn't accidentally evaluate to a vacuous-true gate.
func WithAllRequiredLabels(labels ...string) Option {
	if len(labels) == 0 {
		return func(*Reconciler) {}
	}
	required := slices.Clone(labels)
	return func(r *Reconciler) {
		r.labelPredicates = append(r.labelPredicates, func(i *Issue, _ []string) bool {
			for _, l := range required {
				if !i.HasLabel(l) {
					return false
				}
			}
			return true
		})
	}
}

// WithoutLabel skips issues carrying the given label (case-insensitive,
// matching Issue.HasLabel). Useful for opt-out labels like
// "skip:linear-materializer". Multiple WithoutLabel options compose by AND.
func WithoutLabel(label string) Option {
	return func(r *Reconciler) {
		r.labelPredicates = append(r.labelPredicates, func(i *Issue, _ []string) bool {
			return !i.HasLabel(label)
		})
	}
}

// WithLabelPredicate adds an arbitrary predicate over the issue's label names.
// The predicate receives label names verbatim (no case folding). Returning
// false skips the issue. Multiple predicates compose by AND with each other
// and with WithAllRequiredLabels / WithoutLabel.
//
// The label slice is computed once per Reconcile and shared across all
// predicates, so registering many WithLabelPredicate options is cheap.
//
// Prefer WithAllRequiredLabels and WithoutLabel for the common cases since
// they share Issue.HasLabel's case-insensitive comparison.
func WithLabelPredicate(fn func(labels []string) bool) Option {
	return func(r *Reconciler) {
		r.labelPredicates = append(r.labelPredicates, func(_ *Issue, labels []string) bool {
			return fn(labels)
		})
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

	// Resolve bot identity. Retry on transient failures — Linear's OAuth
	// endpoint can return 401s when multiple instances request tokens
	// concurrently during a fresh deploy, and failing startup on the first
	// such error takes down the whole Cloud Run revision.
	var viewer *User
	var err error
	for attempt := range 5 {
		viewer, err = client.GetViewer(ctx)
		if err == nil {
			break
		}
		if attempt == 4 {
			break
		}
		backoff := time.Duration(attempt+1) * time.Second
		clog.WarnContextf(ctx, "Resolving bot identity failed (attempt %d/5): %v — retrying in %v", attempt+1, err, backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	if err != nil {
		return nil, fmt.Errorf("resolving bot identity after 5 attempts: %w", err)
	}
	client.BotUserID = viewer.ID

	clog.InfoContextf(ctx, "Linear reconciler bot user: %s (%s)", viewer.Name, viewer.ID)

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

	if len(r.requiredLabels) > 0 && !slices.ContainsFunc(r.requiredLabels, issue.HasLabel) {
		log.With("required_labels", r.requiredLabels).Infof("Issue missing all required labels, skipping")
		return nil
	}

	if len(r.labelPredicates) > 0 {
		labels := issue.LabelNames()
		for _, pred := range r.labelPredicates {
			if !pred(issue, labels) {
				log.With("labels", labels).Infof("Issue rejected by label predicate, skipping")
				return nil
			}
		}
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
	clog.InfoContextf(ctx, "Processing Linear issue: %s (priority: %d)", req.Key, req.Priority)

	err := r.Reconcile(ctx, req.Key)
	if err != nil {
		if delay, floor, ok := workqueue.GetRequeueOptions(err); ok {
			clog.InfoContextf(ctx, "Reconciliation requested requeue after %v (floor=%t) for key: %s", delay, floor, req.Key)
			return &workqueue.ProcessResponse{
				RequeueAfterSeconds: int64(delay.Seconds()),
				RequeueFloor:        floor,
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
