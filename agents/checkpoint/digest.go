/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrConfigDrift signals that a suspended envelope cannot be safely resumed
// because the live executor configuration no longer matches the one that
// produced it (a Provider, Model, or ConfigDigest mismatch on wake). Callers
// treat it as "rebuild from scratch" rather than resuming against stale state:
// a resumer returns it wrapped, and a waker (PR9) extracts it with errors.Is to
// fall back to a fresh run and delete the stale checkpoint.
var ErrConfigDrift = errors.New("checkpoint: config drift; rebuild from scratch")

// DigestJSON returns a stable "sha256:<hex>" digest of v's JSON encoding, for
// stamping Envelope.ConfigDigest. On wake, a resume compares the stored digest
// against a freshly computed one over the current configuration; a mismatch
// means the agent's config drifted under the pause and the run must be rebuilt
// from scratch rather than resumed against stale state. Include every input
// whose drift must block a resume — model parameters, tool definitions, and
// the provider SDK version — so all drift funnels into this single enforced
// gate instead of a family of parallel field checks.
//
// The digest is only as stable as encoding/json's field ordering (struct field
// order, sorted map keys), which is deterministic for a fixed Go type. Feed it
// a struct, not an arbitrary map with volatile key insertion, for a meaningful
// comparison across process restarts.
func DigestJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("checkpoint: digest marshal: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
