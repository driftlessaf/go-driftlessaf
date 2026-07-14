/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// recorder-schemas exposes the BigQuery schema for agent trace CloudEvents.
// Pass this output to the cloudevent-recorder module's types variable to
// archive traces from a driftlessaf agent to BigQuery.
//
// Usage:
//
//   module "agenttrace" {
//     source = "path/to/public/go-driftlessaf/agents/agenttrace/iac"
//   }
//
//   module "trace-recorder" {
//     source = "path/to/public/terraform-infra-common/modules/cloudevent-recorder"
//     ...
//     types = module.agenttrace.recorder-schemas
//   }
output "recorder-schemas" {
  value = {
    "dev.chainguard.driftlessaf.agent.trace.v1" = {
      schema          = file("${path.module}/schemas/agent_trace.schema.json")
      partition_field = "start_time"
    }
    // Per-turn LLM payload events. Partitioned on `recorded_at` so per-day
    // retention policies prune naturally; clustered on agent_name and
    // model_id so the common "by-agent" / "by-model" filters are cheap.
    //
    // No retention is configured here. Span payloads are persisted verbatim
    // (see the "Retention" godoc on agenttrace.SpanEventType). Callers
    // wiring this schema into cloudevent-recorder choose between the
    // dataset-wide `retention-period` (days) and the per-event
    // `retention_period_days` override on this map entry — both land on the
    // BigQuery table's partition expiration.
    "dev.chainguard.driftlessaf.agent.span.v1" = {
      schema          = file("${path.module}/schemas/agent_trace_span.schema.json")
      partition_field = "recorded_at"
      clustering      = ["agent_name", "model_id"]
    }
    // Reconciler state-machine transitions (metareconciler). One row per
    // Status/FailureMode change, emitted framework-side; bots opt in by
    // wiring WithStateTransitionEmission from their main. The provider
    // column discriminates metareconciler flavors ("linear" today) so a
    // future GitHub-side emitter shares this table without a v2 event type.
    // Partitioned on transition_at; clustered on bot so per-bot dashboard
    // queries (current state, stuck-in-state) stay cheap.
    "dev.chainguard.driftlessaf.state.transition.v1" = {
      schema          = file("${path.module}/schemas/state_transition.schema.json")
      partition_field = "transition_at"
      clustering      = ["bot"]
    }
  }
}
