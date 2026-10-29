//go:build integration

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsign_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
	siggit "github.com/sigstore/gitsign/pkg/git"
)

// This opt-in test verifies an existing real Google-signed commit using the
// production Fulcio/TUF/Rekor path. It does not sign or write any remote refs.
func TestLiveCommitSignature(t *testing.T) {
	file, identity := os.Getenv("GITSIGN_SIGNED_COMMIT_FILE"), os.Getenv("GITSIGN_EXPECTED_SIGNER")
	if file == "" || identity == "" {
		t.Skip("set GITSIGN_SIGNED_COMMIT_FILE to a GitHub Git-commit JSON response and GITSIGN_EXPECTED_SIGNER")
	}
	r, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var input struct {
		SHA          string                              `json:"sha"`
		Verification struct{ Payload, Signature string } `json:"verification"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 2<<20)).Decode(&input); err != nil {
		t.Fatal(err)
	}
	raw, err := siggit.JoinCommit(&siggit.CommitSig{Payload: []byte(input.Verification.Payload), Gpgsig: []byte(input.Verification.Signature)})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.CommitObject)
	if _, err := obj.Write(raw); err != nil {
		t.Fatal(err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if hash.String() != input.SHA {
		t.Fatalf("reconstructed object: got = %s, want = %s", hash, input.SHA)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := gitsign.VerifyCommit(ctx, repo, hash, identity); err != nil {
		t.Fatal(err)
	}
	if err := gitsign.VerifyCommit(ctx, repo, hash, "wrong-signer@fixture-project.iam.gserviceaccount.com"); err == nil {
		t.Fatal("real signature accepted for wrong identity")
	}
	t.Logf("verified real commit %s for %s; wrong identity rejected", hash, identity)
}
