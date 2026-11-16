# Meta path reconciler

This package runs an analyzer and an agent for repository paths, publishing
changes as pull requests or diagnostics as issues. The pull-request reconciler
iterates on CI and review findings through a change-manager session.

## Pull requests at the commit limit

For a PR without merge conflicts or findings, pending checks finish before the
reconciler decides whether the PR is ready for review. Once mergeability is
known and checks pass, it applies `<identity>/ready-for-review` even when the
PR has reached its commit limit. If give-up comments are configured, this also
clears the give-up label and comment from an earlier attempt.

A PR with findings remains subject to the commit limit, including when other
checks are pending. Unresolved reviews can renew the budget when dynamic commit
budgets are enabled. Merge conflicts continue to trigger a fresh attempt from
the default branch.

Run the package tests from the `public/go-driftlessaf` module:

```sh
go test -race ./reconcilers/githubreconciler/metapathreconciler
```
