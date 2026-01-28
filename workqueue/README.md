# DriftlesAF workqueue

The DriftlesAF workqueue provides a GCS-backed state persistence with retry,
exponential backoff, and concurrency control.

The workqueue is used with the reconciler bots found in `/reconcilers` and the
agentic AI infrastructure tools found in `/agents`.
