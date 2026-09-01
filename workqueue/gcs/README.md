# GCS Workqueue Implementation

This package implements a Google Cloud Storage (GCS) backed workqueue that provides reliable, persistent task processing with state management.

## Bucket Organization

The GCS workqueue uses object prefixes to organize tasks by their state within a single bucket:

### Prefixes

- **`queued/`** - Tasks waiting to be processed
- **`in-progress/`** - Tasks currently being processed by a worker
- **`dead-letter/`** - Tasks that have failed after exceeding maximum retry attempts

### State Transitions

```
queued/{key} → in-progress/{key} → [completed] (deleted)
                      ↓
                 dead-letter/{key} (on failure)
                      ↑
                 queued/{key} (on requeue)
```

## Object Metadata

Each object stores metadata to track task state:

- **`priority`** - Zero-padded 8-digit priority for lexicographic ordering (higher = processed first)
- **`attempts`** - Number of processing attempts
- **`lease-expiration`** - When the current lease expires (for in-progress tasks)
- **`not-before`** - Earliest time the task should be processed (RFC3339 format)
- **`failed-time`** - When the task was moved to dead letter queue (RFC3339 format)
- **`last-attempted`** - Unix timestamp of last processing attempt

## Key Features

- **Priority-based processing** - Higher priority tasks processed first
- **Lease-based ownership** - In-progress tasks have renewable leases to prevent multiple workers processing the same task
- **Automatic retry with backoff** - Failed tasks automatically requeued with exponential backoff
- **Dead letter handling** - Tasks exceeding retry limits moved to dead letter queue
- **Orphan detection** - Detects and handles tasks with expired leases
- **Deduplication** - Duplicate queue requests update priority/timing instead of creating duplicates

## Producer backpressure

`QueuedDepth(ctx, client, queueName, limit)` counts the keys under `queued/`,
counting no further than `limit`. A result equal to `limit` means there are at
least that many; any smaller result is exact. An error returns a count of zero,
never a partial one — a caller that logged the error and carried on would
otherwise read the keys counted before the failure as headroom.

Use it instead of `Enumerate` for that question. `Enumerate` lists every object
in the bucket, in every state, and sorts as it goes — the dispatcher can afford
that because it runs once per dispatch and needs the whole picture, but a bulk
producer polling it would pay for the size of its own backlog on every check.

**What a call costs.** One list request per 1,000 keys counted, rounded up: GCS
caps a page at 1,000 however large `maxResults` is, so a limit of 2,000 is two
round trips and a limit of 200,000 is two hundred, sequentially, on every poll.
Size a threshold knowing that. Only the object name is selected, so the bytes
stay small even when the round trips do not.

It emits `workqueue_queued_depth_latency_seconds` and
`workqueue_queued_depth_errors_total`, labelled with the `queueName` passed in —
which should be the name the queue was built with, or `""` for a queue with none.
The pair exists because this read is polled: without it a stalled producer looks
the same whether the queue is full and it is throttling correctly, or the depth
read is failing and the caller is swallowing it.

Three things it does not do, all deliberate:

- It counts every queued key, including one whose `not-before` has not arrived: a
  delayed key is work the queue still owes, and reading `not-before` would mean
  fetching each object's metadata.
- It counts nothing in progress and nothing dead-lettered — those are bounded by
  the queue's concurrency and by attention, not by how fast a producer runs.
- It counts **one bucket**. A sharded queue (see `hyperqueue`) gives each shard
  its own workqueue module and so its own bucket, so a count against one of them
  sees 1/N of the depth and reads as headroom. A producer in front of a sharded
  queue has to sum the shards.

A `limit` below 1 is an error. No count satisfies the contract above, and a
threshold misconfigured to zero should be heard about rather than read as either
"always full" or "always room" depending on its sign.

## Metrics

The implementation exports Prometheus metrics for:

- Queue sizes (queued, in-progress, dead-lettered)
- Processing latency and wait times
- Retry attempts and completion rates
- Task priorities and attempt distributions
