/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// maxPageSize is the largest page the GCS JSON API will return, whatever
// maxResults asks for. It is the service's documented cap, and it is what makes
// the round-trip count of a QueuedDepth call a function of the limit.
const maxPageSize = 1000

// QueuedDepth counts the keys waiting in the queue, counting no further than
// limit. A result equal to limit means there are AT LEAST that many; any smaller
// result is exact. An error returns a count of zero, never a partial one.
//
// It exists for producers that have to decide whether to enqueue more. The
// question they are really asking is "is the queue deeper than N", not "how deep
// is it", and the difference matters: a bulk producer polls this, so the cost of
// the answer has to be a function of N rather than of the size of the backlog.
//
// Enumerate is the wrong call for that. It lists every object in the bucket, in
// every state, and sorts as it goes — the dispatcher can afford that because it
// runs once per dispatch and needs the whole picture. A producer holding several
// million keys back cannot.
//
// queueName labels the metrics this emits, and should be the name the queue was
// built with (see WithName). Pass "" for a queue that has none.
//
// # What a call costs
//
// One list request per 1,000 keys counted, rounded up: GCS caps a page at 1,000
// however large maxResults is, so a limit of 2,000 is two round trips and a limit
// of 200,000 is two hundred, sequentially, on every poll. Size a threshold
// knowing that. Only the object name is selected, so the bytes are small even
// when the round trips are not, and the last page may over-fetch — the iterator
// re-reads the page size on every fetch rather than narrowing it to what is left
// to count, so the final request can return up to 999 names the count discards.
//
// Three things it does NOT do, all deliberate:
//
//   - It counts every queued key, including one whose not-before time has not
//     arrived. A delayed key is work the queue still owes, which is what
//     backpressure is about; and reading not-before means fetching each object's
//     metadata, which would cost far more than the answer is worth.
//   - It counts nothing in progress and nothing dead-lettered. Those are bounded
//     by the queue's concurrency and by attention respectively, not by how fast
//     a producer runs.
//   - It counts ONE bucket. A sharded queue (see the hyperqueue package) gives
//     each shard its own workqueue module and so its own bucket, so a count
//     against one of them sees 1/N of the depth and reads as headroom. A producer
//     in front of a sharded queue has to sum the shards.
//
// A limit below 1 is an error rather than a defined answer. There is no count
// that satisfies the contract above, and a producer whose threshold has been
// misconfigured to zero should hear about it: the alternative is a bound that
// reads as either "always full" or "always room" depending on the sign.
func QueuedDepth(ctx context.Context, client ClientInterface, queueName string, limit int) (int, error) {
	labels := depthLabels(queueName)
	start := time.Now()
	defer func() {
		mQueuedDepthLatency.With(labels).Observe(time.Since(start).Seconds())
	}()

	fail := func(err error) (int, error) {
		mQueuedDepthErrors.With(labels).Inc()
		// Zero, never the partial count. A caller that logs the error and carries
		// on would otherwise read the keys counted before the failure as headroom,
		// which is the reading this whole function exists to prevent. Enumerate
		// answers the same way for the same reason.
		return 0, err
	}

	if limit < 1 {
		return fail(fmt.Errorf("limit must be at least 1, got %d", limit))
	}

	query := &storage.Query{Prefix: queuedPrefix}
	// Only the name is read, and asking for only the name is half of what keeps
	// this cheap: without it the API returns every object's full metadata, which
	// is most of the bytes and none of the answer.
	if err := query.SetAttrSelection([]string{"Name"}); err != nil {
		return fail(fmt.Errorf("SetAttrSelection() = %w", err))
	}

	iter := client.Objects(ctx, query)
	// The other half. Without a page size the API sends its default page, so a
	// limit of 2 would still pull back a page of names and the bound would hold
	// over pages rather than over items. min with the service cap because a larger
	// value buys nothing and reads as though it did.
	iter.PageInfo().MaxSize = min(limit, maxPageSize)

	count := 0
	for count < limit {
		if _, err := iter.Next(); err != nil {
			if errors.Is(err, iterator.Done) {
				return count, nil
			}
			return fail(fmt.Errorf("Next() = %w", err))
		}
		count++
	}
	return count, nil
}
