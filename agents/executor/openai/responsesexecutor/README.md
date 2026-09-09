# Native Responses executor

This executor runs the `openai-responses` protocol selected by the model router.
It receives a configured `responses.ResponseService`; it doesn't discover
credentials, select a provider, or infer a protocol from the model name.

```text
Resolved route → typed Responses binding → native streaming conversation
                                              │
                      completed function calls ↓
                          client tools → result validators → final result
                              │
                              └─ function_call_output + native reasoning
                                 in the next request
```

Requests use `store: false` and replay native input/output items instead of
`previous_response_id`. Encrypted reasoning content is preserved on the wire
but removed from opt-in trace payloads. Streaming text and argument deltas are
consumed through the SDK; only a completed response can dispatch tools. An
interrupted stream can't execute a partial function call.

The executor uses the shared terminal-submission gate. Ordinary tools finish
before terminal validators run, including when the terminal call appears first
in a parallel batch. Only an accepted, validated submission commits a result.

## Limits and recovery

- The defaults are 200 turns, 32,768 output tokens per request, and 10 concurrent
  tools. Set concurrency to 1 when tool handlers share unsynchronized state.
- Each request has a three-minute deadline; each execution has a 30-minute
  deadline. A shorter caller deadline takes precedence.
- Requests and individual tool results are limited to 8 MiB. Streams are limited
  to 16 MiB and 32,768 events; completed payloads are limited to 8 MiB.
- A completed response can contain at most 128 function calls. Repeated call IDs
  and unsupported output/event types fail closed before dispatch.
- Three consecutive turns without usable tool work or an accepted submission
  end the execution. This bounds correction of invalid JSON, invalid terminal
  payloads, unknown tools, and refusals.
- HTTP 429, 500, 502, 503, and 504 responses can retry twice before a stream starts.
  Partial streams don't retry: completion and usage may be unknown. The SDK's
  own retries are disabled. Error diagnostics omit provider bodies and headers.

Low, medium, and high reasoning effort are sent directly. XHigh and Max clamp
to high, matching the existing OpenAI-compatible executor's supported scale.
Explicit thinking budgets, sampling controls, explicit prompt-cache boundaries,
suspend/resume, hosted tools, and configurable refusal nudges aren't advertised.

## Observability and validation

Turn traces include the route's provider, logical model, protocol, provider model
ID, input/output tokens, cache-read tokens, and reasoning tokens. Reasoning is
already included in output tokens; cache reads are already included in input
tokens. Don't add either subset again when calculating totals. The nullable
`turns.reasoning_tokens` schema addition must be deployed for BigQuery to retain
that field. This change doesn't add model prices or dashboard charts.

The HTTP/SSE fixture tests exercise the actual SDK decoder, native continuation,
reasoning-state replay, parallel terminal ordering, schema rejection, malformed
JSON recovery, cancellation, attribution, and error redaction. They don't prove
that an AWS account is entitled to a model or that live retention settings are
acceptable. The protected evaluation workflow must check those prerequisites
before inference.
