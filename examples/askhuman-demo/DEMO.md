# askhuman-demo — durable suspend/resume, live

This demo shows a DriftlessAF agent (real Claude on Vertex AI) **suspend**
mid-conversation to ask a human a question, park its entire state on disk (no
process, no lease, no memory holds anything), and later **resume in a
completely different OS process** that knows only the checkpoint path and key
— completing the run with the human's answer injected as the pending tool
result.

Every lifecycle phase is a separate subcommand, so every phase is a separate
process by construction:

```
 ask ──► model calls ask_human ──► SUSPEND ──► checkpoint + question land on disk
                                                     │
 resume (before answer) ──► WakeRearm: nothing runs, nothing is mutated
                                                     │
 answer <text> ──► answer recorded, bound to the question's nonce
                                                     │
 resume ──► WakeResume: CAS-claims the checkpoint, fresh executor,
            injects the framed answer, runs to completion, records consumed
```

In production the wake is driven by the workqueue (`RequeueAfter` + a reply
webhook re-enqueueing the key); here you play the dispatcher by invoking
`resume` yourself — which is exactly what makes the tri-state visible.

The stores are the local-file demo grade: a `jsonlstore` append-only log for
the envelopes and a JSON file for the question/answer transport. The
GCS-backed checkpoint store (KMS-sealed envelopes, object-generation CAS)
lands with the `gcsstore` slice of DEV-2247; swapping it in changes only the
two store constructors in `main`. Suspend/resume is wired for the Claude
backend only today, so `AGENT_MODEL` must be `claude-*` (the Gemini and
OpenAI-compatible backends gain it with their executor slices).

## Prerequisites

- `gcloud` authenticated with Application Default Credentials:

  ```bash
  gcloud auth login
  gcloud auth application-default login
  ```

- Vertex AI enabled in the project, with Claude available in Model Garden.

## Build

```bash
cd public/go-driftlessaf/examples
go build -o /tmp/demo ./askhuman-demo
```

## Configuration

| Env var | Required | Default | Meaning |
|---|---|---|---|
| `GCP_PROJECT_ID` | yes | — | Vertex AI project |
| `GCP_REGION` | no | `global` | Vertex AI region (`global` serves Claude) |
| `AGENT_MODEL` | no | `claude-sonnet-4-6` | Model (must be `claude-*`) |
| `CHECKPOINT_PATH` | no | `/tmp/askhuman-demo/checkpoints.jsonl` | jsonlstore envelope log |
| `QUESTIONS_PATH` | no | `/tmp/askhuman-demo/questions.json` | question/answer transport file |
| `DEMO_KEY` | no | `deploy/billing-api` | The workqueue-style key being parked |

```bash
export GCP_PROJECT_ID=<your-project>
```

## The demo script

Recommended terminal layout for a recording: commands in the left pane, and in
a right pane a live view of the state directory so the audience watches state
appear, persist, and get consumed:

```bash
watch -n2 'ls -l /tmp/askhuman-demo/ 2>/dev/null; echo; jq . /tmp/askhuman-demo/questions.json 2>/dev/null'
```

### 1. Start the agent — it suspends

```bash
/tmp/demo ask
```

Expected: the model calls `ask_human` on turn 0 and the process **exits**:

```
▶ PROCESS 88545 — starting agent run (model claude-sonnet-4-6, project …)
▶ model called ask_human — agent SUSPENDED at turn 0
▶ checkpoint (/tmp/askhuman-demo/checkpoints.jsonl) + question (/tmp/askhuman-demo/questions.json)
  persisted — parked (workqueue would requeue in 1m0s; no process holds any state now)
```

Narration beat: *"the process is gone — kill the laptop if you like; the run
lives entirely in those two files."*

### 2. Inspect the parked state

```bash
/tmp/demo status
```

Prints the envelope (provider, model, config digest, remaining turn budget,
park deadline, the pending tool call with its provider-assigned `toolu_…` ID)
and the question — including the model's actual question text as `prompt`.

### 3. Wake before anyone answered — the cheap re-arm

```bash
/tmp/demo resume
```

```
▶ WakeRearm — question still unanswered; nothing executed, nothing mutated, would requeue in 1m0s
```

Narration beat: *"this is what every poll wake costs while a human thinks —
one small read, zero model calls, zero mutations."*

### 4. The human answers

```bash
/tmp/demo answer staging
```

The answer is written next to the question, **bound to the question's nonce**
— an answer to a stale, superseded question can never be injected.

### 5. Wake again — claim, resume, complete

```bash
/tmp/demo resume
```

```
▶ WakeResume — answer present; checkpoint CLAIMED via CAS (generation token), question consumed
… Resuming suspended Claude agent execution … linked_trace=<the suspended run's trace id>
▶ RESUMED RUN COMPLETED: environment="staging" summary="Deployment of billing-api v1.4.2 to staging."
```

A brand-new process rebuilt a fresh executor from the envelope alone, verified
the config digest fail-closed, injected the framed answer as the pending tool
result, and the model completed — quoting the human's answer. The Claude
resume also exercises the cache_control strip/reseed (prompt caching is on by
default; a verbatim replay of the parked transcript would exceed the API's
four-breakpoint limit).

### Reset / re-run

```bash
/tmp/demo clean               # removes the parked state for DEMO_KEY
rm -rf /tmp/askhuman-demo     # full reset
```

## What this demo intentionally does not show

The question/answer transport is a demo-grade local file (a real deployment
uses a webhook/CLI/GitHub-issue `QuestionStore` that also re-enqueues the key
to wake it early); the wake cadence is driven by hand instead of the workqueue
dispatcher; and the envelope is stored unsealed in a local jsonlstore
(production parks in the GCS store with KMS-sealed envelopes — the `gcsstore`
slice). The suspend/resume mechanics — post-quiesce suspension, the envelope,
fail-closed digest, park deadline, CAS claim, nonce-bound answers, framed
injection — are the real library code paths.
