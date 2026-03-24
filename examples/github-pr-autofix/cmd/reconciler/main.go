/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/examples/prvalidation"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"github.com/chainguard-dev/clog"
	_ "github.com/chainguard-dev/clog/gcp/init"
	"github.com/google/go-github/v84/github"
)

type config struct {
	// Agent configuration
	EnableAutofix  bool   `env:"ENABLE_AUTOFIX,default=false"`
	AutofixLabel   string `env:"AUTOFIX_LABEL,default=driftlessaf/autofix"`
	GCPProjectID   string `env:"GCP_PROJECT_ID"`
	GCPRegion      string `env:"GCP_REGION,default=us-central1"`
	Model          string `env:"AGENT_MODEL,default=gemini-2.5-flash"`
	MaxFixAttempts int    `env:"MAX_FIX_ATTEMPTS,default=2"`
}

func New(ctx context.Context, identity string, _ githubreconciler.TokenSourceFunc, cfg config) (githubreconciler.ReconcilerFunc, error) {
	sm, err := statusmanager.NewStatusManager[prvalidation.Details](ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("creating status manager: %w", err)
	}

	var agent metaagent.Agent[*PRContext, *PRFixResult, PRTools]
	if cfg.EnableAutofix {
		if cfg.GCPProjectID == "" {
			return nil, fmt.Errorf("GCP_PROJECT_ID is required when ENABLE_AUTOFIX=true")
		}
		agent, err = newPRFixerAgent(ctx, &cfg)
		if err != nil {
			return nil, fmt.Errorf("creating agent: %w", err)
		}
		clog.InfoContextf(ctx, "Agent enabled with model %s", cfg.Model)
	}

	return func(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
		return reconcilePR(ctx, res, gh, sm, &cfg, agent)
	}, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := githubreconciler.RepoMain(ctx, New); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
	}
}

// hasLabel checks if the PR has a specific label.
func hasLabel(pr *github.PullRequest, labelName string) bool {
	for _, label := range pr.Labels {
		if label.GetName() == labelName {
			return true
		}
	}
	return false
}

// reconcilePR validates a PR and optionally uses an agent to fix issues.
func reconcilePR(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, sm *statusmanager.StatusManager[prvalidation.Details], cfg *config, agent metaagent.Agent[*PRContext, *PRFixResult, PRTools]) error {
	clog.InfoContextf(ctx, "Validating PR: %s/%s#%d", res.Owner, res.Repo, res.Number)

	pr, _, err := gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("fetching PR: %w", err)
	}

	if pr.GetState() == "closed" {
		clog.InfoContext(ctx, "Skipping closed PR")
		return nil
	}

	sha := pr.GetHead().GetSHA()
	title := pr.GetTitle()
	body := pr.GetBody()
	generation := prvalidation.ComputeGeneration(sha, title, body)

	session := sm.NewSession(gh, res, sha)

	observed, err := session.ObservedState(ctx)
	if err != nil {
		return fmt.Errorf("getting observed state: %w", err)
	}
	hasAutofixLabel := hasLabel(pr, cfg.AutofixLabel)
	if observed != nil && observed.Status == "completed" && observed.Details.Generation == generation {
		if observed.Details.AgentEnabled || !hasAutofixLabel {
			clog.InfoContextf(ctx, "Already processed generation %s, skipping", generation[:8])
			return nil
		}
		clog.InfoContextf(ctx, "Label %q added since last run, re-processing", cfg.AutofixLabel)
	}

	titleValid, descValid, issues := prvalidation.ValidatePR(title, body)

	if len(issues) == 0 {
		return session.SetActualState(ctx, "All checks passed!", &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "success",
			Details:    prvalidation.Details{Generation: generation, TitleValid: true, DescriptionValid: true},
		})
	}

	if !cfg.EnableAutofix || agent == nil {
		return session.SetActualState(ctx, fmt.Sprintf("Found %d issue(s)", len(issues)), &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details:    prvalidation.Details{Generation: generation, TitleValid: titleValid, DescriptionValid: descValid, Issues: issues},
		})
	}

	if !hasAutofixLabel {
		clog.InfoContextf(ctx, "Skipping agent - %q label not present", cfg.AutofixLabel)
		return session.SetActualState(ctx, fmt.Sprintf("Found %d issue(s) - add %q label to auto-fix", len(issues), cfg.AutofixLabel), &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details:    prvalidation.Details{Generation: generation, TitleValid: titleValid, DescriptionValid: descValid, Issues: issues, AgentEnabled: false},
		})
	}

	fixAttempts := 0
	if observed != nil && observed.Details.Generation == generation {
		fixAttempts = observed.Details.FixAttempts
	}
	if fixAttempts >= cfg.MaxFixAttempts {
		clog.InfoContextf(ctx, "Max fix attempts (%d) reached, failing", cfg.MaxFixAttempts)
		return session.SetActualState(ctx, "Max fix attempts reached", &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details: prvalidation.Details{
				Generation: generation, TitleValid: titleValid, DescriptionValid: descValid,
				Issues: issues, AgentEnabled: true, FixAttempts: fixAttempts, ModelUsed: cfg.Model,
				AgentReasoning: "Maximum fix attempts reached without successful resolution",
			},
		})
	}

	if err := session.SetActualState(ctx, "Agent fixing issues...", &statusmanager.Status[prvalidation.Details]{
		Status:  "in_progress",
		Details: prvalidation.Details{Generation: generation, Issues: issues, AgentEnabled: true, FixAttempts: fixAttempts + 1, ModelUsed: cfg.Model},
	}); err != nil {
		return fmt.Errorf("setting in_progress status: %w", err)
	}

	changedFiles, err := getChangedFiles(ctx, gh, res.Owner, res.Repo, res.Number)
	if err != nil {
		clog.WarnContext(ctx, "Failed to fetch changed files, continuing without them", "error", err)
		changedFiles = nil
	}

	prContext := &PRContext{Owner: res.Owner, Repo: res.Repo, PRNumber: res.Number, Title: title, Body: body, Issues: issues, ChangedFiles: changedFiles}
	prTools := NewPRTools(gh, res.Owner, res.Repo, res.Number)
	result, err := agent.Execute(ctx, prContext, prTools)
	if err != nil {
		clog.ErrorContext(ctx, "Agent execution failed", "error", err)
		return session.SetActualState(ctx, "Agent failed", &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details: prvalidation.Details{
				Generation: generation, TitleValid: titleValid, DescriptionValid: descValid,
				Issues: issues, AgentEnabled: true, FixAttempts: fixAttempts + 1, ModelUsed: cfg.Model,
				AgentReasoning: fmt.Sprintf("Agent execution error: %v", err),
			},
		})
	}

	pr, _, err = gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("re-fetching PR after agent: %w", err)
	}

	newTitle := pr.GetTitle()
	newBody := pr.GetBody()
	newTitleValid, newDescValid, newIssues := prvalidation.ValidatePR(newTitle, newBody)
	newGeneration := prvalidation.ComputeGeneration(sha, newTitle, newBody)

	conclusion := "success"
	summary := "All checks passed!"
	if len(newIssues) > 0 {
		conclusion = "failure"
		summary = fmt.Sprintf("Found %d issue(s) after agent fixes", len(newIssues))
	}

	return session.SetActualState(ctx, summary, &statusmanager.Status[prvalidation.Details]{
		Status:     "completed",
		Conclusion: conclusion,
		Details: prvalidation.Details{
			Generation: newGeneration, TitleValid: newTitleValid, DescriptionValid: newDescValid,
			Issues: newIssues, AgentEnabled: true, FixesApplied: result.FixesApplied,
			AgentReasoning: result.Reasoning, FixAttempts: fixAttempts + 1, ModelUsed: cfg.Model,
		},
	})
}

// getChangedFiles fetches the list of files changed in the PR.
func getChangedFiles(ctx context.Context, gh *github.Client, owner, repo string, prNumber int) ([]string, error) {
	files, _, err := gh.PullRequests.ListFiles(ctx, owner, repo, prNumber, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, err
	}
	filenames := make([]string, 0, len(files))
	for _, f := range files {
		filenames = append(filenames, f.GetFilename())
	}
	return filenames, nil
}
