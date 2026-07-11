# flynn-ci

Shared, reusable CI and lint/scan configuration for [ionalpha](https://github.com/ionalpha) Go
repositories (`flynn`, `flynn-extensions`, and future modules).

The point is a **single source of truth**. Every consuming repo *calls* the workflow here
instead of copying job logic, and the lint and secret-scan config live in this same repo and
are checked out at run time. So the gates are defined once: they cannot drift or silently
weaken between repos, which is a security property, not just tidiness.

## What's here

| Path | Purpose |
|------|---------|
| `.github/workflows/go-ci.yml` | Reusable Go CI: format/lint, race tests across platforms, `govulncheck`, gitleaks secret scan, and a single `CI success` gate to require in branch protection. |
| `.golangci.yml` | Shared golangci-lint config (gofumpt + goimports + a strict linter set). Org-level import prefix, so it works for every ionalpha module unchanged. |
| `.gitleaks.toml` | Shared gitleaks config (full default ruleset). |

## Using it from another repo

Add a thin caller workflow. It runs the shared jobs against *your* checked-out module; you
keep no CI logic of your own:

```yaml
# .github/workflows/ci.yml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
permissions:
  contents: read
jobs:
  go:
    uses: ionalpha/flynn-ci/.github/workflows/go-ci.yml@main
    with:
      # Pin ci-ref to the same commit you pin the `uses:` line to, so the workflow and the
      # config it loads move together and are both reproducible.
      ci-ref: main
```

Then require the **`CI success`** check in branch protection.

### Inputs

| Input | Default | Notes |
|-------|---------|-------|
| `go-version` | `1.26` | Go toolchain. |
| `platforms` | `["ubuntu-latest","macos-latest","windows-latest"]` | JSON array of test runners. |
| `ci-ref` | `main` | Ref of this repo to load shared config from; pin to match the `uses:` ref. |

## Pinning

For a supply-chain-tight setup, pin the `uses:` line to a tag or commit SHA (not `@main`) and
set `ci-ref` to the same ref, so a change here is adopted deliberately, never silently. The
actions inside this workflow are already pinned to commit SHAs and kept current by Dependabot
in the consuming repos.

## Repo-specific rules

This config holds only rules that apply to **every** ionalpha Go module. A repo with its own
architecture boundaries (layer direction, egress/inbound gates, determinism forbids) layers
those on top in its own tree; they do not belong here.
