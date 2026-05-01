/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package resync_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/resync"
	"chainguard.dev/driftlessaf/workqueue"
	"google.golang.org/grpc"
)

// exampleClient is a minimal workqueue.Client used for documentation only.
// Production code obtains a client via workqueue.NewWorkqueueClient.
type exampleClient struct {
	workqueue.WorkqueueServiceClient

	mu       sync.Mutex
	enqueued []string
}

func (c *exampleClient) Process(_ context.Context, req *workqueue.ProcessRequest, _ ...grpc.CallOption) (*workqueue.ProcessResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enqueued = append(c.enqueued, fmt.Sprintf("%s @ %ds", req.GetKey(), req.GetDelaySeconds()))
	return &workqueue.ProcessResponse{}, nil
}

func (c *exampleClient) Close() error { return nil }

func Example() {
	// Each cron firing constructs its own Sharder with the job-level
	// configuration: now, the cron tick (= shard size = floor), and the
	// workqueue client. Per source (e.g. a repo), call Process with the
	// source's resync period and the candidate keyset; only the slice that
	// falls in the current shard is enqueued, with a within-tick delay.
	keys := map[string]struct{}{
		"alpha":   {},
		"bravo":   {},
		"charlie": {},
		"delta":   {},
		"echo":    {},
		"foxtrot": {},
		"golf":    {},
		"hotel":   {},
	}

	// Walk every tick of one period and report the enqueued keys in order.
	// Across the 24 hourly firings of one period, every key is enqueued
	// exactly once. Across periods, the per-key minute-of-period
	// assignment reshuffles, so the delays change cycle-to-cycle.
	walkPeriod := func(label string, periodStart time.Time) {
		wq := &exampleClient{}
		for hour := range 24 {
			now := periodStart.Add(time.Duration(hour) * time.Hour)
			sh, err := resync.New(time.Hour, wq, resync.WithNow(func() time.Time { return now }))
			if err != nil {
				fmt.Println(err)
				return
			}
			if err := sh.Process(context.Background(), 24*time.Hour, keys); err != nil {
				fmt.Println(err)
				return
			}
		}
		sort.Strings(wq.enqueued)
		fmt.Println(label)
		for _, e := range wq.enqueued {
			fmt.Println(e)
		}
	}

	walkPeriod("=== day 1 ===", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	walkPeriod("=== day 2 ===", time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC))
	// Output:
	// === day 1 ===
	// alpha @ 1260s
	// bravo @ 2160s
	// charlie @ 2880s
	// delta @ 1560s
	// echo @ 420s
	// foxtrot @ 840s
	// golf @ 2760s
	// hotel @ 2100s
	// === day 2 ===
	// alpha @ 540s
	// bravo @ 2640s
	// charlie @ 540s
	// delta @ 1560s
	// echo @ 540s
	// foxtrot @ 840s
	// golf @ 3180s
	// hotel @ 3540s
}
