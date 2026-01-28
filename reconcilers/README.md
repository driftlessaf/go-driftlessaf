# DriftlesAF reconcilers

This directory contains reconciler bots for use with DriftlessAF.

They can be used independently but are best used together with the DriftlessAF
workqueue found in `workqueue` and the agentic AI infrastructure found in
`/agents`.

The following reconciler bots are available:

* `apkreconciler`: A reconciler bot for APK packages as used in Wolfi and Chainguard OS.
* `githubreconciler`: A reconciler bot for GitHub repository data including
  commits, tags, and content.
* `ocireconciler`: A reconciler bot for OCI container images.
