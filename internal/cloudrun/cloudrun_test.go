/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package cloudrun

import "testing"

func TestServiceName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
		job     string
		want    string
	}{
		{name: "service only", service: "my-service", want: "my-service"},
		{name: "job only", job: "my-job", want: "my-job"},
		{name: "service wins over job", service: "my-service", job: "my-job", want: "my-service"},
		{name: "neither set", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("K_SERVICE", tc.service)
			t.Setenv("CLOUD_RUN_JOB", tc.job)

			if got := ServiceName(); got != tc.want {
				t.Errorf("ServiceName(): got = %q, want = %q", got, tc.want)
			}
		})
	}
}

func TestRevisionName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		revision  string
		execution string
		want      string
	}{
		{name: "revision only", revision: "my-revision", want: "my-revision"},
		{name: "execution only", execution: "my-execution", want: "my-execution"},
		{name: "revision wins over execution", revision: "my-revision", execution: "my-execution", want: "my-revision"},
		{name: "neither set", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("K_REVISION", tc.revision)
			t.Setenv("CLOUD_RUN_EXECUTION", tc.execution)

			if got := RevisionName(); got != tc.want {
				t.Errorf("RevisionName(): got = %q, want = %q", got, tc.want)
			}
		})
	}
}

func TestIsJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  string
		want bool
	}{
		{name: "job set", job: "my-job", want: true},
		{name: "job unset", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUD_RUN_JOB", tc.job)

			if got := IsJob(); got != tc.want {
				t.Errorf("IsJob(): got = %v, want = %v", got, tc.want)
			}
		})
	}
}
