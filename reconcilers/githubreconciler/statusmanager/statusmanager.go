/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"

	"cloud.google.com/go/compute/metadata"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v84/github"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	internaltemplate "chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
)

// Status represents the overall status of reconciliation
type Status[T any] struct {
	// ObservedGeneration is the last commit SHA that was fully processed
	ObservedGeneration string `json:"observedGeneration"`

	// Status represents the current status of the check run
	// Can be: "queued", "in_progress", or "completed"
	Status string `json:"status"`

	// Conclusion represents the conclusion when status is "completed"
	// Can be: "action_required", "cancelled", "failure", "neutral",
	// "success", "skipped", "stale", or "timed_out"
	Conclusion string `json:"conclusion,omitempty"`

	// Details contains reconciler-specific state data
	Details T `json:"details"`
}

// StatusManager manages reconciliation status via GitHub Check Runs
type StatusManager[T any] struct {
	identity         string
	projectID        string
	serviceName      string
	readOnly         bool
	detailsURLFunc   DetailsURLFunc
	templateExecutor *internaltemplate.Template[Status[T]]
}

// DetailsURLFunc builds the "Details" link attached to a reconciler's check run
// for the given resource and SHA. Returning an empty string omits the link,
// which is appropriate for externally-facing bots whose operators' logs (e.g.
// the default Cloud Logging query) are not accessible to the repositories the
// bot runs against.
type DetailsURLFunc func(res *githubreconciler.Resource, sha string) string

// Option configures a StatusManager.
type Option func(*config)

type config struct {
	detailsURL DetailsURLFunc
}

// WithDetailsURL overrides how the check run "Details" link is built. By default
// the link points at the reconciler's Cloud Logging query in the GCP console.
func WithDetailsURL(fn DetailsURLFunc) Option {
	return func(c *config) { c.detailsURL = fn }
}

// WithoutDetailsURL omits the check run "Details" link entirely. Use this for
// externally-facing reconcilers, since the default Cloud Logging link is not
// accessible to external repositories.
func WithoutDetailsURL() Option {
	return WithDetailsURL(func(*githubreconciler.Resource, string) string { return "" })
}

// NewStatusManager creates a new status manager with the given identity
func NewStatusManager[T any](ctx context.Context, identity string, opts ...Option) (*StatusManager[T], error) {
	return newStatusManager[T](ctx, identity, false, opts...)
}

// NewReadOnlyStatusManager creates a new read-only status manager with the given identity.
// A read-only status manager will fail any operations that attempt to mutate GitHub state.
func NewReadOnlyStatusManager[T any](ctx context.Context, identity string, opts ...Option) (*StatusManager[T], error) {
	return newStatusManager[T](ctx, identity, true, opts...)
}

func newStatusManager[T any](ctx context.Context, identity string, readOnly bool, opts ...Option) (*StatusManager[T], error) {
	// Get project ID from metadata
	projectID, err := metadata.ProjectIDWithContext(ctx)
	if err != nil {
		clog.InfoContextf(ctx, "Unable to detect project-id: %v", err)
	}

	// Get service name once at startup
	serviceName, ok := os.LookupEnv("K_SERVICE")
	if !ok {
		clog.InfoContextf(ctx, "Unable to detect reconciler service: %v", err)
	}

	templateExecutor, err := internaltemplate.New[Status[T]](identity, "-status", "status")
	if err != nil {
		return nil, fmt.Errorf("creating template executor: %w", err)
	}

	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	return &StatusManager[T]{
		identity:         identity,
		projectID:        projectID,
		serviceName:      serviceName,
		readOnly:         readOnly,
		detailsURLFunc:   cfg.detailsURL,
		templateExecutor: templateExecutor,
	}, nil
}

// Session represents a reconciliation session for a specific resource
type Session[T any] struct {
	manager  *StatusManager[T]
	client   *github.Client
	resource *githubreconciler.Resource
	sha      string
	readOnly bool

	mu         sync.Mutex
	checkRunID *int64 // Set when we find an existing check run
}

// NewSession creates a new reconciliation session for a GitHub resource and SHA.
// The resource provides owner, repo, and URL (used as key for log filtering).
// The SHA is the commit to attach check runs to.
func (sm *StatusManager[T]) NewSession(client *github.Client, res *githubreconciler.Resource, sha string) *Session[T] {
	return &Session[T]{
		manager:  sm,
		client:   client,
		resource: res,
		sha:      sha,
		readOnly: sm.readOnly,
	}
}

// getCheckRunID returns the stored check run ID if set
func (s *Session[T]) getCheckRunID() *int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkRunID
}

// setCheckRunID stores a check run ID for future updates
func (s *Session[T]) setCheckRunID(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkRunID = &id
}

// buildDetailsURL builds the check run "Details" link for this resource and SHA.
// When a custom DetailsURLFunc is configured it is used (and may return "" to
// omit the link); otherwise it defaults to the reconciler's Cloud Logging query.
func (s *Session[T]) buildDetailsURL() string {
	if s.manager.detailsURLFunc != nil {
		return s.manager.detailsURLFunc(s.resource, s.sha)
	}

	// Build the Cloud Logging URL with both key and SHA filters
	// The query filters for:
	// - Cloud Run revision logs
	// - Specific service name
	// - The resource key (PR URL) in jsonPayload.key
	// - The SHA in jsonPayload.sha
	query := fmt.Sprintf(`resource.type="cloud_run_revision"
resource.labels.service_name=%q
jsonPayload.key=%q
jsonPayload.sha=%q`,
		s.manager.serviceName,
		s.resource.URL,
		s.sha,
	)

	encodedQuery := url.QueryEscape(query)

	return fmt.Sprintf(
		"https://console.cloud.google.com/logs/query;query=%s;storageScope=project;summaryFields=:false:32:beginning;duration=P2D?project=%s",
		encodedQuery,
		s.manager.projectID,
	)
}

// ObservedState retrieves the last observed state for the current SHA
func (s *Session[T]) ObservedState(ctx context.Context) (*Status[T], error) {
	name, err := checkRunName(s.manager.identity, s.resource)
	if err != nil {
		return nil, err
	}

	// Get check runs for this SHA
	checkRuns, _, err := s.client.Checks.ListCheckRunsForRef(
		ctx, s.resource.Owner, s.resource.Repo, s.sha,
		&github.ListCheckRunsOptions{
			CheckName: github.Ptr(name),
		})

	if err != nil {
		return nil, fmt.Errorf("listing check runs: %w", err)
	}

	// Find our check run
	for _, run := range checkRuns.CheckRuns {
		if run.GetName() == name {
			// Record the check run ID for potential updates
			s.setCheckRunID(run.GetID())

			// Extract status from output
			return s.manager.extractStatusFromOutput(run.Output)
		}
	}

	return nil, nil // No status found
}

// ObservedStateAtSHA retrieves the status for a specific commit SHA without creating a session.
// This is useful for gathering historical status across multiple commits.
func (sm *StatusManager[T]) ObservedStateAtSHA(
	ctx context.Context,
	client *github.Client,
	res *githubreconciler.Resource,
	sha string,
) (*Status[T], error) {
	name, err := checkRunName(sm.identity, res)
	if err != nil {
		return nil, err
	}

	checkRuns, _, err := client.Checks.ListCheckRunsForRef(
		ctx, res.Owner, res.Repo, sha,
		&github.ListCheckRunsOptions{
			CheckName: github.Ptr(name),
		})

	if err != nil {
		return nil, fmt.Errorf("listing check runs: %w", err)
	}

	for _, run := range checkRuns.CheckRuns {
		if run.GetName() == name {
			return sm.extractStatusFromOutput(run.Output)
		}
	}

	return nil, nil
}

// SetActualState updates the state for the current SHA
func (s *Session[T]) SetActualState(ctx context.Context, title string, status *Status[T]) error {
	if s.readOnly {
		return errors.New("cannot set actual state: status manager is read-only")
	}

	name, err := checkRunName(s.manager.identity, s.resource)
	if err != nil {
		return err
	}

	// Ensure ObservedGeneration is set to current SHA
	status.ObservedGeneration = s.sha

	// Build markdown output with embedded JSON
	output, err := s.manager.buildCheckRunOutput(status)
	if err != nil {
		return fmt.Errorf("building output: %w", err)
	}

	// Build the details URL for logs. An empty URL (e.g. an externally-facing
	// bot that opted out) is omitted rather than sent as a blank link.
	var detailsURLPtr *string
	if detailsURL := s.buildDetailsURL(); detailsURL != "" {
		detailsURLPtr = &detailsURL
	}

	// Only pass Conclusion if it's not empty
	var conclusionPtr *string
	if status.Conclusion != "" {
		conclusionPtr = &status.Conclusion
	}

	// Build CheckRunOutput with optional annotations
	checkOutput := &github.CheckRunOutput{
		Title:   github.Ptr(title),
		Summary: github.Ptr(output),
	}

	// Check if Details implements Annotated interface
	if annotated, ok := any(status.Details).(Annotated); ok {
		annotations := annotated.Annotations()
		if len(annotations) > 0 {
			checkOutput.Annotations = annotations
			checkOutput.AnnotationsCount = github.Ptr(len(annotations))
		}
	}

	// Check if we have a check run ID from ObservedState
	if checkRunID := s.getCheckRunID(); checkRunID != nil {
		// Update existing check run
		_, _, err = s.client.Checks.UpdateCheckRun(ctx, s.resource.Owner, s.resource.Repo, *checkRunID, github.UpdateCheckRunOptions{
			Name:       name,
			Status:     &status.Status,
			Conclusion: conclusionPtr,
			DetailsURL: detailsURLPtr,
			Output:     checkOutput,
		})

		if err != nil {
			return fmt.Errorf("updating check run: %w", err)
		}

		return nil
	}

	// Create new check run
	checkRun, _, err := s.client.Checks.CreateCheckRun(ctx, s.resource.Owner, s.resource.Repo, github.CreateCheckRunOptions{
		Name:       name,
		HeadSHA:    s.sha,
		Status:     &status.Status,
		Conclusion: conclusionPtr,
		DetailsURL: detailsURLPtr,
		Output:     checkOutput,
	})

	if err != nil {
		return fmt.Errorf("creating check run: %w", err)
	}

	// Store the ID for future updates
	s.setCheckRunID(checkRun.GetID())

	return nil
}

// ResetForRerun resets a check so its owning reconciler reprocesses the
// resource. It honors a user's "Re-run" click in the GitHub UI (a
// check_run.rerequested webhook).
//
// It creates a fresh check run rather than updating the existing one. A
// completed check run's conclusion is sticky: GitHub will not let an update move
// it back to a non-terminal state, so an in-place update leaves the prior
// red/green state showing. A new check run with the same name and head SHA
// supersedes the old one — GitHub surfaces the latest run per name — resetting
// the displayed state to a pending (queued) state. ListCheckRunsForRef likewise
// defaults to the latest run per name, so the owning reconciler observes this one.
//
// The new run carries a summary with no embedded status marker, so the
// reconciler's next ObservedState reports no observed state (see
// extractStatusFromOutput) and it does the work again, overwriting this
// placeholder.
//
// client must be authenticated as the GitHub App that owns the check.
func ResetForRerun(ctx context.Context, client *github.Client, owner, repo, name, headSHA string) error {
	if _, _, err := client.Checks.CreateCheckRun(ctx, owner, repo, github.CreateCheckRunOptions{
		Name:    name,
		HeadSHA: headSHA,
		Status:  github.Ptr("queued"),
		Output: &github.CheckRunOutput{
			Title:   github.Ptr("Re-run requested"),
			Summary: github.Ptr("Re-run requested; awaiting reprocessing."),
		},
	}); err != nil {
		return fmt.Errorf("creating check run for re-run: %w", err)
	}
	return nil
}

// markdownProvider is an interface for types that can provide markdown representation
type markdownProvider interface {
	Markdown() string
}

// Annotated is an interface for types that can provide GitHub check run annotations
type Annotated interface {
	Annotations() []*github.CheckRunAnnotation
}

// checkRunName returns the check run name for the given identity and resource.
// For pull requests, returns the identity as-is.
// For paths, returns "{identity} ({path})" to distinguish different paths.
// Returns an error if the resource type is not supported by StatusManager.
func checkRunName(identity string, res *githubreconciler.Resource) (string, error) {
	switch res.Type {
	case githubreconciler.ResourceTypePullRequest:
		return identity, nil
	case githubreconciler.ResourceTypePath:
		return fmt.Sprintf("%s (%s)", identity, res.Path), nil
	case githubreconciler.ResourceTypeIssue:
		return "", errors.New("issues are not supported by StatusManager")
	default:
		return "", fmt.Errorf("unrecognized resource type: %s", res.Type)
	}
}

// buildCheckRunOutput builds the markdown output with embedded status data
func (sm *StatusManager[T]) buildCheckRunOutput(status *Status[T]) (string, error) {
	var markdown string

	// Check if Details implements Markdown() method
	if provider, ok := any(status.Details).(markdownProvider); ok {
		// Use the custom markdown representation
		markdown = provider.Markdown()
	}
	// If no Markdown() method or empty output, no visible content

	// Embed status data using the template executor
	return sm.templateExecutor.Embed(markdown, status)
}

// extractStatusFromOutput extracts the embedded status from a check run's output.
// A nil output, or one whose summary carries no parseable embedded status — e.g.
// cleared by a re-run reset, or otherwise corrupt — yields a nil status, which
// the reconciler treats as no observed state and reprocesses. This keeps stored
// status self-healing. The error result is retained for symmetry with the
// Observed* callers; extraction failures are deliberately reported as "no
// observed state" rather than surfaced.
//
//nolint:unparam // see above: error is always nil by design
func (sm *StatusManager[T]) extractStatusFromOutput(output *github.CheckRunOutput) (*Status[T], error) {
	if output == nil || output.Summary == nil {
		return nil, nil
	}
	status, err := sm.templateExecutor.Extract(*output.Summary)
	if err != nil {
		return nil, nil //nolint:nilerr // no parseable status means "no observed state"
	}
	return status, nil
}
