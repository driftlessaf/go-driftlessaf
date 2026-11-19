/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gitsign_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign/gitsigntest"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	siggit "github.com/sigstore/gitsign/pkg/git"
)

// signedCommit signs a commit with message and returns it as GitHub's commit
// verification reports it.
func signedCommit(t *testing.T, f *gitsigntest.Fixture, message string) gitsign.SignedCommit {
	t.Helper()
	repo := newRepo(t)
	c := newCommit(t, repo, message)
	sign(t, c, f)
	return gitsign.SignedCommit{
		ID:        storeCommit(t, repo, c).String(),
		Payload:   unsignedEncoding(t, c),
		Signature: []byte(c.PGPSignature),
	}
}

func unsignedEncoding(t *testing.T, c *object.Commit) []byte {
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
	payload, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestIdentifyCommit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*testing.T, *gitsigntest.Fixture, *gitsign.SignedCommit) // applied after signing
		wantErr string
		want    gitsign.Identity
		valid   bool
	}{{
		name:  "valid",
		want:  gitsign.Identity{Issuer: gitsign.GoogleIssuer, Subject: fixtureSigner},
		valid: true,
	}, {
		name: "signature copied from another commit",
		change: func(t *testing.T, f *gitsigntest.Fixture, c *gitsign.SignedCommit) {
			c.Signature = signedCommit(t, f, "another commit\n").Signature
		},
	}, {
		name: "payload of another commit",
		change: func(t *testing.T, f *gitsigntest.Fixture, c *gitsign.SignedCommit) {
			c.Payload = signedCommit(t, f, "another commit\n").Payload
		},
	}, {
		name:   "unsigned",
		change: func(_ *testing.T, _ *gitsigntest.Fixture, c *gitsign.SignedCommit) { c.Signature = nil },
	}, {
		name: "payload and signature exceed 1 MiB",
		change: func(_ *testing.T, _ *gitsigntest.Fixture, c *gitsign.SignedCommit) {
			c.Payload = make([]byte, 1<<20)
		},
		wantErr: "commit requires a bounded gitsign signature",
	}, {
		name: "reconstructed object exceeds 1 MiB",
		change: func(t *testing.T, _ *gitsigntest.Fixture, c *gitsign.SignedCommit) {
			raw, err := siggit.JoinCommit(&siggit.CommitSig{Payload: c.Payload, Gpgsig: c.Signature})
			if err != nil {
				t.Fatal(err)
			}
			padding := (1 << 20) + 1 - len(raw)
			c.Payload = append(c.Payload, bytes.Repeat([]byte("x"), padding)...)
		},
		wantErr: "signed commit must be at most 1 MiB",
	}, {
		name: "missing message separator",
		change: func(_ *testing.T, _ *gitsigntest.Fixture, c *gitsign.SignedCommit) {
			c.Payload, _, _ = bytes.Cut(c.Payload, []byte("\n\n"))
		},
		wantErr: "commit payload has no message separator",
	}, {
		name: "uppercase commit ID",
		change: func(_ *testing.T, _ *gitsigntest.Fixture, c *gitsign.SignedCommit) {
			c.ID = strings.ToUpper(c.ID)
		},
	}, {
		name: "untrusted root",
		change: func(t *testing.T, f *gitsigntest.Fixture, _ *gitsign.SignedCommit) {
			var err error
			if f.Certs, err = siggit.NewCertVerifier(); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name:   "missing receipt",
		change: func(_ *testing.T, f *gitsigntest.Fixture, _ *gitsign.SignedCommit) { clear(f.Receipts) },
	}, {
		name: "expired at integration",
		change: func(_ *testing.T, f *gitsigntest.Fixture, _ *gitsign.SignedCommit) {
			for _, e := range f.Receipts {
				e.IntegratedTime = new(f.Cert.NotAfter.Add(time.Second).Unix())
			}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := gitsigntest.New(t, fixtureSigner, gitsign.GoogleIssuer)
			commit := signedCommit(t, f, "export\n")
			if tc.change != nil {
				tc.change(t, f, &commit)
			}
			got, err := gitsign.NewVerifier(f.Certs, f.Inclusion).IdentifyCommit(t.Context(), commit)
			if !tc.valid {
				if err == nil {
					t.Fatalf("IdentifyCommit: got = %+v, want an error", got)
				}
				if tc.wantErr != "" && err.Error() != tc.wantErr {
					t.Fatalf("IdentifyCommit: got error %q, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("IdentifyCommit: got = %v, want = nil", err)
			}
			if got != tc.want {
				t.Fatalf("IdentifyCommit: got = %+v, want = %+v", got, tc.want)
			}
		})
	}
}

// A signer may put the gpgsig header before other headers, and the payload
// that GitHub reports then has those headers after the signature's position.
func TestIdentifyCommitFindsSignaturePosition(t *testing.T) {
	f := gitsigntest.New(t, fixtureSigner, gitsign.GoogleIssuer)
	repo := newRepo(t)
	c := newCommit(t, repo, "export\n")
	payload := unsignedEncoding(t, c)
	headers, message, _ := bytes.Cut(payload, []byte("\n\n"))
	payload = append(append(headers, "\nencoding ISO-8859-1\n\n"...), message...)
	sig, err := f.Sign(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	gpgsig := append(append([]byte("gpgsig "), bytes.ReplaceAll(bytes.TrimSuffix(sig, []byte("\n")), []byte("\n"), []byte("\n "))...), '\n')
	raw := append(append(append([]byte{}, headers...), '\n'), gpgsig...)
	raw = append(append(raw, "encoding ISO-8859-1\n\n"...), message...)
	commit := gitsign.SignedCommit{
		ID:        plumbing.ComputeHash(plumbing.CommitObject, raw).String(),
		Payload:   payload,
		Signature: sig,
	}
	got, err := gitsign.NewVerifier(f.Certs, f.Inclusion).IdentifyCommit(t.Context(), commit)
	if err != nil {
		t.Fatalf("IdentifyCommit: got = %v, want = nil", err)
	}
	if want := (gitsign.Identity{Issuer: gitsign.GoogleIssuer, Subject: fixtureSigner}); got != want {
		t.Fatalf("IdentifyCommit: got = %+v, want = %+v", got, want)
	}
}

func TestDefaultVerifierReportsTrustRootFailure(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocked)
	_, err := gitsign.NewDefaultVerifier(t.Context())
	if _, ok := errors.AsType[*gitsign.TrustRootError](err); !ok {
		t.Fatalf("NewDefaultVerifier() error = %v, want TrustRootError", err)
	}
}

// failingTransport fails every request with err.
type failingTransport struct {
	err error
}

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

func TestDefaultVerifierFetchesWithClient(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	refused := errors.New("refused by the caller's transport")

	_, err := gitsign.NewDefaultVerifier(t.Context(), gitsign.WithHTTPClient(&http.Client{Transport: failingTransport{err: refused}}))
	if _, ok := errors.AsType[*gitsign.TrustRootError](err); !ok {
		t.Fatalf("NewDefaultVerifier() error = %v, want TrustRootError", err)
	}
	if !errors.Is(err, refused) {
		t.Fatalf("NewDefaultVerifier() error = %v, want the client's transport error", err)
	}
}

// cancellingTransport cancels the caller's context when a request starts,
// then waits for the request's context to end.
type cancellingTransport struct {
	cancel context.CancelFunc
}

func (c cancellingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.cancel()
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestDefaultVerifierStopsFetchWhenContextEnds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, err := gitsign.NewDefaultVerifier(ctx, gitsign.WithHTTPClient(&http.Client{Transport: cancellingTransport{cancel: cancel}}))
	if _, ok := errors.AsType[*gitsign.TrustRootError](err); !ok {
		t.Fatalf("NewDefaultVerifier() error = %v, want TrustRootError", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewDefaultVerifier() error = %v, want context.Canceled", err)
	}
}

func TestIdentifyCommitBoundsSignatureSearch(t *testing.T) {
	f := gitsigntest.New(t, fixtureSigner, gitsign.GoogleIssuer)
	v := gitsign.NewVerifier(f.Certs, f.Inclusion)
	c := signedCommit(t, f, "export\n")
	headers, message, _ := bytes.Cut(c.Payload, []byte("\n\n"))
	c.Payload = bytes.Join([][]byte{headers, []byte(strings.Repeat("extra value\n", 64)), message}, []byte("\n"))
	var err error
	c.Signature, err = f.Sign(bytes.NewReader(c.Payload))
	if err != nil {
		t.Fatal(err)
	}
	data := &siggit.CommitSig{Payload: c.Payload, Gpgsig: c.Signature}
	raw, err := siggit.JoinCommit(data)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = plumbing.ComputeHash(plumbing.CommitObject, raw).String()
	got, err := v.IdentifyCommit(t.Context(), c)
	if want := (gitsign.Identity{Issuer: gitsign.GoogleIssuer, Subject: fixtureSigner}); err != nil || got != want {
		t.Fatalf("IdentifyCommit with the signature last = %v, %v, want %v", got, err, want)
	}
	c.ID = strings.Repeat("0", 40)
	if _, err := v.IdentifyCommit(t.Context(), c); err == nil || !strings.Contains(err.Error(), "too many possible signature positions") {
		t.Fatalf("IdentifyCommit() error = %v, want signature search limit", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := v.IdentifyCommit(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("IdentifyCommit() error = %v, want context.Canceled", err)
	}
}
