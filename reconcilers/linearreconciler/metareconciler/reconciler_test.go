/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/workqueue"
)

// fakePREventHandler records HandlePREvent invocations for routing assertions.
type fakePREventHandler struct {
	calls []string
}

func (f *fakePREventHandler) HandlePREvent(_ context.Context, prURL string) (*workqueue.ProcessResponse, error) {
	f.calls = append(f.calls, prURL)
	return &workqueue.ProcessResponse{}, nil
}

// fakeLinearServer records Process invocations.
type fakeLinearServer struct {
	workqueue.UnimplementedWorkqueueServiceServer
	keys []string
	err  error
}

func (f *fakeLinearServer) Process(_ context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	f.keys = append(f.keys, req.Key)
	return &workqueue.ProcessResponse{}, f.err
}

func TestDualKeyServer_Process_RoutesByKeyShape(t *testing.T) {
	tests := []struct {
		name           string
		key            string
		wantPRCalls    []string
		wantLinearKeys []string
	}{
		{
			name:           "GitHub PR URL routes to HandlePREvent",
			key:            "https://github.com/owner/repo/pull/42",
			wantPRCalls:    []string{"https://github.com/owner/repo/pull/42"},
			wantLinearKeys: nil,
		},
		{
			name:           "Linear UUID routes to linearRec",
			key:            "c4ce4cdb-de9c-43e8-87a3-32d2db38eaae",
			wantPRCalls:    nil,
			wantLinearKeys: []string{"c4ce4cdb-de9c-43e8-87a3-32d2db38eaae"},
		},
		{
			name:           "Linear identifier (ENG-123) routes to linearRec",
			key:            "ENG-123",
			wantPRCalls:    nil,
			wantLinearKeys: []string{"ENG-123"},
		},
		{
			name:           "GitHub issue URL is dropped (not routed to linearRec)",
			key:            "https://github.com/owner/repo/issues/7",
			wantPRCalls:    nil,
			wantLinearKeys: nil,
		},
		{
			name:           "GitHub commit URL is dropped",
			key:            "https://github.com/owner/repo/commit/abc1234",
			wantPRCalls:    nil,
			wantLinearKeys: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakePR := &fakePREventHandler{}
			fakeLin := &fakeLinearServer{}
			d := &dualKeyServer{metaRec: fakePR, linearRec: fakeLin}

			if _, err := d.Process(t.Context(), &workqueue.ProcessRequest{Key: tc.key}); err != nil {
				t.Fatalf("Process: %v", err)
			}
			if !equalStrSlices(fakePR.calls, tc.wantPRCalls) {
				t.Errorf("HandlePREvent calls = %v, want %v", fakePR.calls, tc.wantPRCalls)
			}
			if !equalStrSlices(fakeLin.keys, tc.wantLinearKeys) {
				t.Errorf("linearRec keys = %v, want %v", fakeLin.keys, tc.wantLinearKeys)
			}
		})
	}
}

func TestDualKeyServer_Process_PropagatesLinearError(t *testing.T) {
	wantErr := errors.New("boom")
	d := &dualKeyServer{
		metaRec:   &fakePREventHandler{},
		linearRec: &fakeLinearServer{err: wantErr},
	}
	if _, err := d.Process(t.Context(), &workqueue.ProcessRequest{Key: "ENG-1"}); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
