/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"strings"

	"github.com/chainguard-dev/clog"
)

const (
	// maxPRBodyBytes is GitHub's limit on a pull request body. GitHub counts
	// characters; a byte count is at least as strict, so a body within this
	// bound is accepted.
	maxPRBodyBytes = 65536
	// detailsOpen and detailsClose delimit a collapsible block. A cut inside
	// one would swallow the rest of the body into the block.
	detailsOpen  = "<details"
	detailsClose = "\n</details>"
)

// fitPRBody joins the rendered template body, the fixed suffix (operator note
// and trace footer), and the embedded data block, truncating the rendered part
// when the whole would exceed maxPRBodyBytes. The suffix and the data block are
// never cut: the data block is what Extract reads back on the next reconcile.
// Every <details> block the cut leaves open is closed so the remainder renders.
func fitPRBody(ctx context.Context, rendered, suffix, tail string) string {
	budget := maxPRBodyBytes - len(suffix) - len(tail)
	if len(rendered) <= budget {
		return rendered + suffix + tail
	}
	// Reserve room for the marker and for closing every block the cut could
	// leave open, so the result stays within budget.
	reserve := len(truncationMarker) + strings.Count(rendered, detailsOpen)*len(detailsClose)
	cut := truncateOnRune(rendered, max(budget-reserve, 0))
	cut += strings.Repeat(detailsClose, max(strings.Count(cut, detailsOpen)-strings.Count(cut, "</details>"), 0))
	clog.WarnContext(ctx, "PR body exceeds GitHub's size limit, truncated the rendered template",
		"rendered_bytes", len(rendered), "kept_bytes", len(cut), "fixed_bytes", len(suffix)+len(tail))
	return cut + suffix + tail
}
