/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/examples/prvalidation"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	_ "github.com/chainguard-dev/clog/gcp/init"
	"github.com/google/go-github/v88/github"
)

type config struct {
	// Agent configuration
	EnableAutofix  bool   `env:"ENABLE_AUTOFIX,default=false"`
	AutofixLabel   string `env:"AUTOFIX_LABEL,default=driftlessaf/autofix"`
	GCPProjectID   string `env:"GCP_PROJECT_ID"`
	GCPRegion      string `env:"GCP_REGION,default=us-central1"`
	Model          string `env:"AGENT_MODEL,default=gemini-2.5-flash"`
	MaxFixAttempts int    `env:"MAX_FIX_ATTEMPTS,default=2"`

	// Ask-human (suspend/resume) demo configuration. Opt-in and off by
	// default; when enabled the agent advertises an ask_human suspend tool, and
	// the reconciler drives the checkpoint/suspend lifecycle (park on suspend,
	// poll on wake, resume on answer). Requires a claude-* AGENT_MODEL:
	// suspend/resume is only wired for the Claude backend today.
	//
	// CHECKPOINT_BUCKET selects the durable path: envelopes park in the bucket
	// via gcsstore, and the human transport is GitHub itself (githubquestions)
	// — the question is posted as a comment on the PR being reconciled, and a
	// repository collaborator answers by replying "/answer <text>". Parked
	// runs survive restarts, redeploys, and scale-out.
	//
	// WARNING — NOT DURABLE without CHECKPOINT_BUCKET: the fallback is a
	// node-local file (ASK_HUMAN_SNAPSHOT_PATH, defaults under /tmp) plus an
	// in-memory question store. Parked runs and any human answers already
	// collected DO NOT survive a pod restart, redeploy, or a second replica: a
	// wake that finds no checkpoint silently falls back to a fresh run,
	// discarding the parked conversation. That path is demo-grade only.
	EnableAskHuman       bool          `env:"ENABLE_ASK_HUMAN,default=false"`
	AskHumanBucket       string        `env:"CHECKPOINT_BUCKET"`
	AskHumanWakeInterval time.Duration `env:"ASK_HUMAN_WAKE_INTERVAL,default=30s"`
	AskHumanSnapshotPath string        `env:"ASK_HUMAN_SNAPSHOT_PATH,default=/tmp/ask-human-checkpoints.jsonl"`
}

func New(ctx context.Context, identity string, clients *githubreconciler.ClientCache, cfg config) (githubreconciler.ReconcilerFunc, error) {
	sm, err := statusmanager.NewStatusManager[prvalidation.Details](ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("creating status manager: %w", err)
	}

	if cfg.EnableAskHuman && !cfg.EnableAutofix {
		clog.WarnContextf(ctx, "ENABLE_ASK_HUMAN=true has no effect without ENABLE_AUTOFIX=true: the ask-human lifecycle is part of the autofix agent, and only the plain validation path will run")
	}

	var agent metaagent.Agent[*PRContext, *PRFixResult, PRTools]
	var askHuman *askHumanReconciler
	if cfg.EnableAutofix {
		if cfg.GCPProjectID == "" {
			return nil, fmt.Errorf("GCP_PROJECT_ID is required when ENABLE_AUTOFIX=true")
		}
		if cfg.EnableAskHuman {
			// Ask-human path: the agent is built fresh per wake by the
			// reconciler (a meta-agent binds its executor once at construction,
			// so a resume needs a fresh one), so no long-lived agent is captured
			// here.
			askHuman, err = newAskHumanReconciler(ctx, &cfg, clients)
			if err != nil {
				return nil, fmt.Errorf("creating ask-human reconciler: %w", err)
			}
			clog.InfoContextf(ctx, "Ask-human suspend/resume enabled with model %s", cfg.Model)
			if cfg.AskHumanBucket != "" {
				clog.InfoContextf(ctx, "Ask-human state is durable: checkpoints in GCS bucket %s under %s/, questions as PR comments answered with %s", cfg.AskHumanBucket, askHumanCheckpointPrefix, "/answer")
			} else {
				clog.WarnContextf(ctx, "Ask-human state is NOT durable: checkpoints live in the node-local file %s and answers in memory; parked runs will not survive a restart or redeploy, and with more than one replica a wake routed to another replica re-runs the agent from scratch (a duplicate model run and a burned fix attempt) while the original parked conversation is orphaned (demo only — set CHECKPOINT_BUCKET for the durable path)", cfg.AskHumanSnapshotPath)
			}
		} else {
			agent, err = newPRFixerAgent(ctx, &cfg)
			if err != nil {
				return nil, fmt.Errorf("creating agent: %w", err)
			}
			clog.InfoContextf(ctx, "Agent enabled with model %s", cfg.Model)
		}
	}

	return func(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
		return reconcilePR(ctx, res, gh, sm, &cfg, agent, askHuman)
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

// errMaxFixAttempts signals that beginAttempt found the fix-attempt budget
// exhausted and has already recorded the terminal "Max fix attempts reached"
// status; callers translate it into a clean (nil) reconcile return rather
// than a failure.
var errMaxFixAttempts = errors.New("max fix attempts reached")

// reconcilePR validates a PR and optionally uses an agent to fix issues.
func reconcilePR(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, sm *statusmanager.StatusManager[prvalidation.Details], cfg *config, agent metaagent.Agent[*PRContext, *PRFixResult, PRTools], askHuman *askHumanReconciler) error {
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

	if !cfg.EnableAutofix || (agent == nil && askHuman == nil) {
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

	// attemptsRecorded is the FixAttempts value the terminal status writes
	// carry. It stays at the observed count unless this reconcile begins a
	// fresh execution: an ask-human resume completes the attempt its parked
	// fresh run already recorded, so it must not re-increment.
	attemptsRecorded := fixAttempts
	// beginAttempt gates on MaxFixAttempts and records the attempt as an
	// in_progress status write. The plain path runs it before every execution;
	// the ask-human path defers it into run()'s WakeFresh branch so that a
	// rearm poll (which re-enters this reconcile every wake interval) burns no
	// fix attempt and writes no status, and an answered resume is never
	// blocked by the attempts gate — a parked run must stay resumable even at
	// the cap, or a checkpoint would be orphaned before the human answers.
	beginAttempt := func(ctx context.Context) error {
		if fixAttempts >= cfg.MaxFixAttempts {
			clog.InfoContextf(ctx, "Max fix attempts (%d) reached, failing", cfg.MaxFixAttempts)
			if err := session.SetActualState(ctx, "Max fix attempts reached", &statusmanager.Status[prvalidation.Details]{
				Status:     "completed",
				Conclusion: "failure",
				Details: prvalidation.Details{
					Generation: generation, TitleValid: titleValid, DescriptionValid: descValid,
					Issues: issues, AgentEnabled: true, FixAttempts: fixAttempts, ModelUsed: cfg.Model,
					AgentReasoning: "Maximum fix attempts reached without successful resolution",
				},
			}); err != nil {
				return err
			}
			return errMaxFixAttempts
		}
		attemptsRecorded = fixAttempts + 1
		if err := session.SetActualState(ctx, "Agent fixing issues...", &statusmanager.Status[prvalidation.Details]{
			Status:  "in_progress",
			Details: prvalidation.Details{Generation: generation, Issues: issues, AgentEnabled: true, FixAttempts: attemptsRecorded, ModelUsed: cfg.Model},
		}); err != nil {
			return fmt.Errorf("setting in_progress status: %w", err)
		}
		return nil
	}

	if askHuman == nil {
		if err := beginAttempt(ctx); err != nil {
			if errors.Is(err, errMaxFixAttempts) {
				return nil
			}
			return err
		}
	}

	changedFiles, err := getChangedFiles(ctx, gh, res.Owner, res.Repo, res.Number)
	if err != nil {
		clog.WarnContext(ctx, "Failed to fetch changed files, continuing without them", "error", err)
		changedFiles = nil
	}

	prContext := &PRContext{Owner: res.Owner, Repo: res.Repo, PRNumber: res.Number, Title: title, Body: body, Issues: issues, ChangedFiles: changedFiles}
	prTools := NewPRTools(gh, res.Owner, res.Repo, res.Number)
	// The ask-human path drives the checkpoint/suspend lifecycle: a suspension
	// or an unanswered wake surfaces as a workqueue requeue error (the pause
	// signal). A completed resume returns a result like a plain run.
	var result *PRFixResult
	if askHuman != nil {
		result, err = askHuman.run(ctx, res.String(), prContext, prTools, beginAttempt)
		// A requeue is the pause signal, not a failure: propagate it untouched so
		// the key parks (or re-polls) without recording a failed run or burning a
		// fix attempt.
		if _, isRequeue := workqueue.GetRequeueDelay(err); isRequeue {
			return err
		}
		// beginAttempt already wrote the terminal max-attempts status inside
		// run()'s WakeFresh branch; nothing further to record.
		if errors.Is(err, errMaxFixAttempts) {
			return nil
		}
		// A resume failure happened AFTER the wake claimed the envelope and
		// consumed the answer. Writing a terminal failure status would end
		// the key's processing with both silently dropped — return the error
		// so the workqueue retries; the retry finds no checkpoint and re-runs
		// the fix from scratch.
		if errors.Is(err, errResumeFailed) {
			return err
		}
	} else {
		result, err = agent.Execute(ctx, prContext, prTools)
	}
	if err != nil {
		clog.ErrorContext(ctx, "Agent execution failed", "error", err)
		return session.SetActualState(ctx, "Agent failed", &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details: prvalidation.Details{
				Generation: generation, TitleValid: titleValid, DescriptionValid: descValid,
				Issues: issues, AgentEnabled: true, FixAttempts: attemptsRecorded, ModelUsed: cfg.Model,
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
			AgentReasoning: result.Reasoning, FixAttempts: attemptsRecorded, ModelUsed: cfg.Model,
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
