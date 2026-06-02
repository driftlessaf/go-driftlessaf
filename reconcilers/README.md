# DriftlesAF reconcilers

This directory contains reconciler bots for use with DriftlessAF.

They can be used independently but are best used together with the DriftlessAF
workqueue found in `workqueue` and the agentic AI infrastructure found in
`/agents`.

The following reconciler bots are available:

* `apkreconciler`: A reconciler bot for APK packages as used in Wolfi and Chainguard OS.
* `githubreconciler`: A reconciler bot for GitHub repository data including
  commits, tags, and content.
  * `branchreconciler`: An iterative, criteria-driven reconciler for automated workflows on Git branches without pull requests.
  * `metareconciler`: GitHub issue-to-PR reconciler with agent iteration and human review.
  * `metapathreconciler`: GitHub path-to-PR reconciler with analysis and diagnostic fixing.
* `linearreconciler`: A reconciler bot for Linear issue tracking integration.
* `ocireconciler`: A reconciler bot for OCI container images.
