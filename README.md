# go-ci

Shared, reusable CI and lint/scan configuration for [ionalpha](https://github.com/ionalpha) Go
repositories (`flynn`, `flynn-extensions`, and future modules).

The point is a **single source of truth**. Every consuming repo *calls* the workflow here
instead of copying job logic, and the lint and secret-scan config live in this same repo and
are checked out at run time. So the gates are defined once: they cannot drift or silently
weaken between repos, which is a security property, not just tidiness.

## What's here

| Path | Purpose |
|------|---------|
| `.github/workflows/go-ci.yml` | Reusable Go CI: format/lint, race tests across platforms, `govulncheck`, gitleaks secret scan, a PR-body gate, and a single `CI success` gate to require in branch protection. |
| `.github/workflows/codeql.yml` | Reusable CodeQL analysis (Go + Actions). |
| `.github/workflows/scorecard.yml` | Reusable OpenSSF Scorecard supply-chain analysis. |
| `.golangci.yml` | Shared golangci-lint config (gofumpt + goimports + a strict linter set). Org-level import prefix, so it works for every ionalpha module unchanged. |
| `.gitleaks.toml` | Shared gitleaks config (full default ruleset). |

These cover the gates that apply to **every** ionalpha Go repo. Repo-specific gates (a repo's
own e2e suite, semgrep invariant rules, alloc-ceiling benchmarks, fuzz targets, and any
architecture lint rules) stay in that repo, layered on top.

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
    uses: ionalpha/go-ci/.github/workflows/go-ci.yml@main
    with:
      # Pin ci-ref to the same commit you pin the `uses:` line to, so the workflow and the
      # config it loads move together and are both reproducible.
      ci-ref: main
```

Then require the **`CI success`** check in branch protection.

### Security scanners (CodeQL + Scorecard)

They need their own event triggers (schedule, branch-protection) and elevated permissions, so
each is a separate thin caller in the consuming repo:

```yaml
# .github/workflows/codeql.yml
name: CodeQL
on:
  push: { branches: [main] }
  pull_request: { branches: [main] }
  schedule: [{ cron: "27 3 * * 1" }]
permissions:
  contents: read
  security-events: write
jobs:
  codeql:
    uses: ionalpha/go-ci/.github/workflows/codeql.yml@main
```

```yaml
# .github/workflows/scorecard.yml
name: OpenSSF Scorecard
on:
  branch_protection_rule:
  schedule: [{ cron: "21 4 * * 2" }]
  push: { branches: [main] }
permissions:
  contents: read
  security-events: write
  id-token: write
jobs:
  scorecard:
    uses: ionalpha/go-ci/.github/workflows/scorecard.yml@main
```

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
