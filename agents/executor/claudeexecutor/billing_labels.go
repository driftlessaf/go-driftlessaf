/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"maps"
	"regexp"
	"slices"
)

// maxVertexLabels is Vertex AI's limit on labels per partner-model request.
const maxVertexLabels = 32

var (
	vertexLabelKey   = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	vertexLabelValue = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)

	// attributionLabels are kept first when labels exceed maxVertexLabels.
	attributionLabels = []string{"service_name", "team", "product"}

	// ownerLabels name an owner from the attribution list. Billing reads a
	// value that is not on the list as Unknown ahead of the project tag, so an
	// empty or "unknown" placeholder is never sent.
	ownerLabels = []string{"team", "product"}
)

// vertexBillingLabels returns the resource labels that meet Vertex AI's rules
// for partner-model requests: keys and values use only lowercase letters,
// numbers, underscores, and dashes. Labels outside those rules are dropped, as
// are team and product labels whose value is empty or "unknown". Beyond
// maxVertexLabels, the attribution labels are kept and the rest are taken in
// key order.
func vertexBillingLabels(labels map[string]string) map[string]string {
	valid := make(map[string]string, len(labels))
	for k, v := range labels {
		if !vertexLabelKey.MatchString(k) || !vertexLabelValue.MatchString(v) {
			continue
		}
		if slices.Contains(ownerLabels, k) && (v == "" || v == "unknown") {
			continue
		}
		valid[k] = v
	}
	if len(valid) <= maxVertexLabels {
		return valid
	}
	keys := make([]string, 0, len(valid))
	for _, k := range attributionLabels {
		if _, ok := valid[k]; ok {
			keys = append(keys, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(valid)) {
		if !slices.Contains(attributionLabels, k) {
			keys = append(keys, k)
		}
	}
	kept := make(map[string]string, maxVertexLabels)
	for _, k := range keys[:maxVertexLabels] {
		kept[k] = valid[k]
	}
	return kept
}
