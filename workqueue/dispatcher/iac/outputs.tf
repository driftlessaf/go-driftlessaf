terraform {
  required_version = ">= 1.9"
}

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// recorder-schemas exposes the BigQuery schema for workqueue dispatcher error
// CloudEvents. Pass this output to the cloudevent-recorder module's types
// variable to archive dispatcher error events to BigQuery.
//
// Usage:
//
//   module "workqueue-error" {
//     source = "path/to/public/go-driftlessaf/workqueue/dispatcher/iac"
//   }
//
//   module "error-recorder" {
//     source = "path/to/public/terraform-infra-common/modules/cloudevent-recorder"
//     ...
//     types = module.workqueue-error.recorder-schemas
//   }
output "recorder-schemas" {
  value = {
    "dev.chainguard.workqueue.error.v1" = {
      schema          = file("${path.module}/schemas/workqueue_error.schema.json")
      partition_field = "occurAt"
      // Table is still under active development; disable delete protection so
      // the schema can be iterated on without the schema-deletion playbook.
      deletion_protection = false
    }
  }
}
