/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsign_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign/gitsigntest"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	siggit "github.com/sigstore/gitsign/pkg/git"
)

const fixtureSigner = "signer@fixture-project.iam.gserviceaccount.com"

func newRepo(t *testing.T) *gogit.Repository {
	t.Helper()
	repo, err := gogit.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// storeCommit writes c, including any signature, and returns its hash.
func storeCommit(t *testing.T, repo *gogit.Repository, c *object.Commit) plumbing.Hash {
	t.Helper()
	obj := repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		t.Fatal(err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// newCommit returns an unstored commit of an empty tree with message.
func newCommit(t *testing.T, repo *gogit.Repository, message string) *object.Commit {
	t.Helper()
	tree := repo.Storer.NewEncodedObject()
	if err := (&object.Tree{}).Encode(tree); err != nil {
		t.Fatal(err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(tree)
	if err != nil {
		t.Fatal(err)
	}
	who := object.Signature{Name: "Fixture", Email: fixtureSigner, When: time.Unix(1700000000, 0).UTC()}
	return &object.Commit{Author: who, Committer: who, Message: message, TreeHash: treeHash}
}

// sign sets c's signature to signer's signature over c's unsigned encoding.
func sign(t *testing.T, c *object.Commit, signer gogit.Signer) {
	t.Helper()
	obj := &plumbing.MemoryObject{}
	if err := c.EncodeWithoutSignature(obj); err != nil {
		t.Fatal(err)
	}
	r, err := obj.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sig, err := signer.Sign(r)
	if err != nil {
		t.Fatal(err)
	}
	c.PGPSignature = string(sig)
}

func TestVerifyCommit(t *testing.T) {
	for _, tc := range []struct {
		name             string
		identity, issuer string // certificate SAN and issuer; default fixtureSigner and GoogleIssuer
		expected         string // identity passed to VerifyCommit; default fixtureSigner
		emptyExpected    bool
		unsigned         bool
		change           func(*object.Commit)                   // applied after signing
		setup            func(*testing.T, *gitsigntest.Fixture) // applied after signing
		wantErr          string
	}{{
		name: "valid",
	}, {
		name:     "unsigned",
		unsigned: true,
		wantErr:  "requires a bounded gitsign signature",
	}, {
		name:    "malformed",
		change:  func(c *object.Commit) { c.PGPSignature = "not a CMS signature\n" },
		wantErr: "verifying commit signature",
	}, {
		name:     "wrong identity",
		identity: "publish-wolfi@fixture-project.iam.gserviceaccount.com",
		wantErr:  "verifying signer identity",
	}, {
		name:    "wrong issuer",
		issuer:  "https://attacker.example",
		wantErr: "verifying signer identity",
	}, {
		name:     "expected signer is not a service account",
		identity: "person@example.com",
		expected: "person@example.com",
		wantErr:  "expected signer and signature verification are required",
	}, {
		name:          "missing expected signer",
		emptyExpected: true,
		wantErr:       "expected signer and signature verification are required",
	}, {
		name: "untrusted root",
		setup: func(t *testing.T, f *gitsigntest.Fixture) {
			var err error
			if f.Certs, err = siggit.NewCertVerifier(); err != nil {
				t.Fatal(err)
			}
		},
		wantErr: "verifying commit signature",
	}, {
		name:    "missing trust roots",
		setup:   func(_ *testing.T, f *gitsigntest.Fixture) { f.Certs = nil },
		wantErr: "expected signer and signature verification are required",
	}, {
		name:    "tampered payload",
		change:  func(c *object.Commit) { c.Message += "injected message\n" },
		wantErr: "verifying commit signature",
	}, {
		name:    "oversized",
		change:  func(c *object.Commit) { c.Message = strings.Repeat("x", 1<<20+1) },
		wantErr: "at most 1 MiB",
	}, {
		name:    "missing receipt",
		setup:   func(_ *testing.T, f *gitsigntest.Fixture) { clear(f.Receipts) },
		wantErr: "verifying Rekor inclusion",
	}, {
		name: "nil receipt",
		setup: func(_ *testing.T, f *gitsigntest.Fixture) {
			for k := range f.Receipts {
				f.Receipts[k] = nil
			}
		},
		wantErr: "no integrated time",
	}, {
		name: "missing integrated time",
		setup: func(_ *testing.T, f *gitsigntest.Fixture) {
			for _, e := range f.Receipts {
				e.IntegratedTime = nil
			}
		},
		wantErr: "no integrated time",
	}, {
		name: "expired at integration",
		setup: func(_ *testing.T, f *gitsigntest.Fixture) {
			for _, e := range f.Receipts {
				e.IntegratedTime = new(f.Cert.NotAfter.Add(time.Second).Unix())
			}
		},
		wantErr: "outside the signing certificate's validity",
	}, {
		name: "not yet valid at integration",
		setup: func(_ *testing.T, f *gitsigntest.Fixture) {
			for _, e := range f.Receipts {
				e.IntegratedTime = new(f.Cert.NotBefore.Add(-time.Second).Unix())
			}
		},
		wantErr: "outside the signing certificate's validity",
	}, {
		name:    "verification unavailable",
		setup:   func(_ *testing.T, f *gitsigntest.Fixture) { f.ReceiptError = errors.New("Rekor unavailable") },
		wantErr: "verifying Rekor inclusion",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := gitsigntest.New(t, cmp.Or(tc.identity, fixtureSigner), cmp.Or(tc.issuer, gitsign.GoogleIssuer))
			repo := newRepo(t)
			c := newCommit(t, repo, "export\n")
			if !tc.unsigned {
				sign(t, c, f)
			}
			if tc.change != nil {
				tc.change(c)
			}
			hash := storeCommit(t, repo, c)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			expected := cmp.Or(tc.expected, fixtureSigner)
			if tc.emptyExpected {
				expected = ""
			}
			err := f.VerifyCommit(t.Context(), repo, hash, expected)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyCommit: got = %v, want = nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("VerifyCommit: got = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyCommitCancellation(t *testing.T) {
	f := gitsigntest.New(t, fixtureSigner, gitsign.GoogleIssuer)
	repo := newRepo(t)
	c := newCommit(t, repo, "payload")
	sign(t, c, f)
	hash := storeCommit(t, repo, c)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.VerifyCommit(ctx, repo, hash, fixtureSigner); !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyCommit: got = %v, want = context canceled", err)
	}
}

// The production entry point rejects an unsigned commit before it loads any
// trust roots, so this runs without network access.
func TestVerifyCommitRejectsUnsignedOffline(t *testing.T) {
	repo := newRepo(t)
	hash := storeCommit(t, repo, newCommit(t, repo, "unsigned\n"))
	if err := gitsign.VerifyCommit(t.Context(), repo, hash, fixtureSigner); err == nil || !strings.Contains(err.Error(), "requires a bounded gitsign signature") {
		t.Fatalf("VerifyCommit: got = %v, want an unsigned-commit error", err)
	}
}

func TestVerifyCommitPreservesMessageBytes(t *testing.T) {
	for _, message := range []string{"no final newline", "final newline\n", "CRLF\r\nsecond\r\n", "embedded\rcarriage return"} {
		t.Run(message, func(t *testing.T) {
			f := gitsigntest.New(t, fixtureSigner, gitsign.GoogleIssuer)
			repo := newRepo(t)
			c := newCommit(t, repo, message)
			sign(t, c, f)
			if err := f.VerifyCommit(t.Context(), repo, storeCommit(t, repo, c), fixtureSigner); err != nil {
				t.Fatal(err)
			}
			// A newline-only alteration must not verify as the original signed bytes.
			c.Message += "\n"
			if err := f.VerifyCommit(t.Context(), repo, storeCommit(t, repo, c), fixtureSigner); err == nil {
				t.Fatal("message normalization accepted altered commit")
			}
		})
	}
}

func TestVerifyCommitRejectsAmbiguousHeaders(t *testing.T) {
	f := gitsigntest.New(t, fixtureSigner, gitsign.GoogleIssuer)
	repo := newRepo(t)
	c := newCommit(t, repo, "signed\n")
	sign(t, c, f)
	original := storeCommit(t, repo, c)
	obj, err := repo.Storer.EncodedObject(plumbing.CommitObject, original)
	if err != nil {
		t.Fatal(err)
	}
	r, err := obj.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{{
		name:   "duplicate tree",
		change: func(b []byte) []byte { return append([]byte("tree "+c.TreeHash.String()+"\n"), b...) },
	}, {
		name:   "duplicate signature",
		change: func(b []byte) []byte { return append([]byte("gpgsig forged\n"), b...) },
	}, {
		name:   "CRLF headers",
		change: func(b []byte) []byte { return bytes.Replace(b, []byte("\n"), []byte("\r\n"), 1) },
	}} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &plumbing.MemoryObject{}
			obj.SetType(plumbing.CommitObject)
			if _, err := obj.Write(tc.change(append([]byte{}, raw...))); err != nil {
				t.Fatal(err)
			}
			hash, err := repo.Storer.SetEncodedObject(obj)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.VerifyCommit(t.Context(), repo, hash, fixtureSigner); err == nil {
				t.Fatal("ambiguous headers accepted")
			}
		})
	}
}
