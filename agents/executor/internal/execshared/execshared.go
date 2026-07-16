/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"cmp"
	"fmt"
	"maps"
	"os"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/internal/cloudrun"
)

// AppendUserPromptSuffix appends the built suffix to the prompt with a
// blank-line separator. A nil suffix returns the prompt unchanged. The suffix
// must be fully bound; a Build failure (for example an unbound placeholder)
// is returned wrapped.
func AppendUserPromptSuffix(prompt string, suffix *promptbuilder.Prompt) (string, error) {
	if suffix == nil {
		return prompt, nil
	}
	built, err := suffix.Build()
	if err != nil {
		return "", fmt.Errorf("failed to build user prompt suffix: %w", err)
	}
	return prompt + "\n\n" + built, nil
}

// DefaultResourceLabels returns the resource labels for billing and
// observability attribution, starting from defaults derived from environment
// variables:
//   - service_name: from K_SERVICE, falling back to CLOUD_RUN_JOB (defaults to "unknown")
//   - product: from CHAINGUARD_PRODUCT (defaults to "unknown")
//   - team: from CHAINGUARD_TEAM (defaults to "unknown")
//
// Custom labels override defaults if they use the same keys.
//
// Reading the environment here is deliberate: these labels attribute a
// deployment, so the defaults come from deployment-level env vars set by the
// service's infrastructure, mirroring how cloudrun resolves the workload
// identity. The labels parameter is the explicit configuration path for
// callers that need to override or extend them.
func DefaultResourceLabels(labels map[string]string) map[string]string {
	serviceName := cmp.Or(cloudrun.ServiceName(), "unknown")
	productName := os.Getenv("CHAINGUARD_PRODUCT")
	if productName == "" {
		productName = "unknown"
	}
	teamName := os.Getenv("CHAINGUARD_TEAM")
	if teamName == "" {
		teamName = "unknown"
	}

	resourceLabels := map[string]string{
		"service_name": serviceName,
		"product":      productName,
		"team":         teamName,
	}

	// Merge custom labels (these will override defaults if keys match)
	if labels != nil {
		maps.Copy(resourceLabels, labels)
	}
	return resourceLabels
}
