/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// agent_trace_costs is a view over the agent trace table that adds per-row USD
// cost estimates derived from the model and token counts, plus a provider
// column derived from turns[].system ('anthropic' | 'google.vertex' |
// 'openai') so cost can be grouped by serving path. Rates are currently
// provider-agnostic — Anthropic direct and Vertex AI Global list prices match
// for every priced model — but the price table and price resolution carry a
// provider dimension so per-provider rates are a one-line change if they
// diverge; resolution picks exactly one price row per tier, preferring
// provider-specific rows over provider-agnostic (NULL) ones (see the header
// comment in sql/agent_trace_costs.sql).
//
// The view is written into a dataset supplied by the caller so multiple
// derived views can share one dataset and the recorder dataset stays reserved
// for raw event ingest. The view's location is implied by view_dataset_id and
// must match source_dataset_id.
//
// Models recognised today: Claude Fable 5, Claude Opus 4.5-4.8, Claude
// Sonnet/Haiku 4.5+, Gemini 2.0/2.5/3.x families — with or without a Vertex
// '@version' suffix. Unknown models produce NULL costs (visible signal rather
// than silent zero) so that drift in model usage is noticeable.
resource "google_bigquery_table" "agent_trace_costs" {
  project    = var.project_id
  dataset_id = var.view_dataset_id
  table_id   = var.view_table_id

  friendly_name = "Agent traces with cost estimates"
  description   = "Agent traces enriched with USD cost estimates derived from model and token counts, plus a provider column from turns[].system. List pricing (identical on Anthropic direct and Vertex AI Global today); cache writes use 5m TTL rate."

  deletion_protection = var.deletion_protection

  view {
    query = templatefile("${path.module}/sql/agent_trace_costs.sql", {
      project_id      = var.project_id
      dataset_id      = var.source_dataset_id
      source_table_id = var.source_table_id
    })
    use_legacy_sql = false
  }
}
