# Operations runbook

This repository is a public fork of
[`Apollo-Reborn/apollo-backend`](https://github.com/Apollo-Reborn/apollo-backend).
Application setup and service operation remain documented in `README.md` and
the `GETTING_STARTED` guides. This runbook covers repository automation owned
by the fork.

## Validation and release workflows

- `Unit tests and Docker validation` verifies Go modules, loads the PostgreSQL
  test schema, runs race-enabled tests, vets and builds the server, runs
  golangci-lint, and validates the Dockerfile.
- `Build and push Docker image` publishes multi-architecture images to GHCR on
  pushes to `main`, version tags, or an authorized manual dispatch.
- External actions are pinned to immutable commits. Dependabot checks action
  versions weekly and Go modules daily.

Do not rerun or manually dispatch workflows while GitHub reports an account or
spending-limit execution block. Those zero-step failures are provider failures,
not application failures.

## Upstream maintenance

`Fast-forward upstream sync` checks Apollo-Reborn's `main` branch weekly and on
manual request. It updates this fork only when the fork is a strict ancestor of
upstream, validates the candidate, and pushes with an exact lease. It stops
without changing refs when the histories diverge; reconcile downstream-only
changes through review rather than force-pushing or auto-merging them.

## Lifecycle observation

`Workflow lifecycle email` observes validation, image publication, and upstream
sync runs. Delivery uses the public, immutable lifecycle action and the
repository secret `OP_SERVICE_ACCOUNT_TOKEN`; Microsoft Graph credentials stay
in the `Boneman` 1Password vault. The observer has read-only repository access
and ignores source runs from untrusted forks.

## Recovery

For a failed source workflow, inspect the first failed job and reproduce its
documented command locally. For an upstream divergence, compare
`origin/main...authoritative-upstream/main`, open a reviewed reconciliation
change, and retain both histories. Never bypass branch protection, rewrite
shared history, or expose tokens in logs.
