# Recorded Vertex batch output

`replay_test.go` replays output recorded from real Vertex AI batch jobs
through the batch client and the package's HTTP fakes. The fakes reproduce
the line shapes this package expects. A recording checks those shapes
against what Vertex actually writes.

Each provider's tests skip until its directory exists. Fixtures are never
written by hand: a hand-made fixture would only repeat the fakes'
assumptions, which is the gap these tests exist to close.

## Layout

```
testdata/replay/
  claude/
    capture.json
    requests.json
    input.jsonl
    manifest.json
    output/*.jsonl
  gemini/
    (same files)
```

- `capture.json` records how the recording was made:

  ```json
  {
    "model": "claude-opus-5-5@default",
    "location": "us",
    "date": "YYYY-MM-DD",
    "genai_version": "vX.Y.Z",
    "state": "JOB_STATE_SUCCEEDED",
    "unobtained": []
  }
  ```

  `state` is the terminal job state Vertex reported. `unobtained` lists any
  case keys the recording could not elicit. Each one is skipped, with the
  date in the skip message, rather than failed.
- `requests.json` holds the requests that were submitted, as a JSON array:
  - Claude entries are `{"key": ..., "params": <anthropic.MessageNewParams>}`.
  - Gemini entries are `{"key": ..., "contents": [<genai.Content>], "config": <genai.GenerateContentConfig>}`.
- `input.jsonl` and `manifest.json` are the objects that `SubmitClaude` or
  `SubmitGemini` staged under `<prefix>/<id>/`, copied byte for byte. The
  replay re-submits `requests.json` and requires the client to reproduce
  both files exactly.
- `output/*.jsonl` holds every object Vertex wrote under the job's
  `outputInfo.gcsOutputDirectory`, each under its original base name.

## Case keys

Every request's key names the outcome it is meant to elicit. Each case's
request content (prompt, system instruction and settings) must differ from
every other case's. The replay classifies each output line with a fresh
collector, so two identical requests would match the same key by digest,
and `dropped_label` is matched by digest alone.

| Provider | Key | Expected result |
|---|---|---|
| claude | `success` | text, `end_turn`, usage, no error |
| claude | `refusal` | `ErrRefused`, `Refusal` set, no text |
| claude | `max_tokens` | `ErrTruncated`, partial text kept |
| claude | `errored` | per-line error that `IsRetryable` rejects (an invalid request) |
| claude | `missing` | `ErrMissing`, because the replay drops its line |
| gemini | `stop` | text, `STOP`, usage, no error |
| gemini | `max_tokens` | `ErrTruncated`, partial text kept |
| gemini | `safety` | `ErrRefused` with category `SAFETY`, no text |
| gemini | `prompt_blocked` | `ErrRefused` from the prompt `blockReason`, no text |
| gemini | `dropped_label` | text, `STOP`, matched by request digest, because the replay strips its echoed labels |
| gemini | `no_candidates` | per-line error that `IsRetryable` accepts |
| gemini | `missing` | `ErrMissing`, because the replay drops its line |

## Scrub before committing

This directory ships in the public module `chainguard.dev/driftlessaf`.
Before committing a recording:

1. Use synthetic prompts only. Never use customer data, internal content,
   or prompts that would be unsafe to publish.
2. Replace every identifier in every file with these fixed placeholders:
   - project id: `example-project`
   - project number: `0`
   - bucket: `example-bucket`
   - job resource name: `projects/0/locations/us/batchPredictionJobs/0`
3. Remove any email address or service account.
4. Run `go test -run 'TestReplay|TestScrub' .` from the package directory.
   `TestReplayFixturesScrubbed` fails on any project, bucket, job, project
   number or email in these files that is not one of the placeholders.

Scrubbing must not touch request or response content. The replay requires
`input.jsonl` and `manifest.json` to match what the client encodes from
`requests.json`.
