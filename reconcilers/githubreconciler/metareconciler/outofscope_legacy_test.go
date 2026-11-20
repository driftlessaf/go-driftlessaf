/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
)

func TestOutOfScopeLegacyFingerprintReachesAgent(t *testing.T) {
	fx := newOOSFixture(t)
	fx.first(oosFinding)

	current, automated, ok := changemanager.ReviewBodyFingerprint(reviewBody("reviewer[bot]"))
	if !ok || !automated {
		t.Fatal("review body could not be fingerprinted")
	}
	legacy := sha256.Sum256([]byte("reviewer[bot]\n" + oosFinding))
	legacyFingerprint := hex.EncodeToString(legacy[:])
	if current == legacyFingerprint {
		t.Fatal("current fingerprint matches the legacy digest")
	}

	fx.h.mu.Lock()
	updated := strings.Replace(fx.h.prText, current, legacyFingerprint, 1)
	if updated == fx.h.prText {
		fx.h.mu.Unlock()
		t.Fatal("could not find the current fingerprint in PR metadata")
	}
	fx.h.prText = updated
	fx.h.mu.Unlock()

	fx.setReviews(bot(2, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "legacy fingerprint", 2)
}
