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
| `cmd/relkit` | Monorepo release tool: per-component tags, reproducible signed artifacts, import-graph-scoped changelogs. See [Releasing a monorepo](#releasing-a-monorepo-relkit). |
| `.github/workflows/monorepo-release.yml` | Reusable release workflow: builds, signs and publishes the one component named by the tag. |
| `.github/workflows/monorepo-check.yml` | Reusable PR gate: validates the release manifest and reports which components have unreleased changes. |

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

## Releasing a monorepo: `relkit`

A repo that ships several binaries from one module cannot version them together. An
extension that has not changed should not get a new version because a sibling did, and a
consumer who pinned one should not have to re-verify all of them. So each component is
tagged on its own timeline:

```
token/v0.1.0     releases only the token extension
example/v0.2.0   releases only the example extension
```

`cmd/relkit` is the tool that does it, and `.github/workflows/monorepo-release.yml` is the
reusable workflow that runs it on a tag push.

**Why not GoReleaser.** Its open-source edition derives the version from `git describe`, so
`token/v0.1.0` is not a version it can parse; prefixed-tag support (`.PrefixedTag`) is a paid
feature. Coercing OSS into it means fighting version derivation in every artifact name.
Owning the artifact path instead buys the thing no config-driven tool can do: relkit scopes a
component by its **real import graph** (`go list -deps`), not by a path glob. So a fix to a
shared package lands in the changelog of every component that imports it, and no others, and
"which components does this branch oblige me to release" has a correct answer that nobody has
to maintain a list of directories to keep true.

### The manifest

One `.release.yaml` at the module root declares what the repo ships:

```yaml
version: 1
defaults:
  goos: [linux, darwin, windows]
  goarch: [amd64, arm64]
  env: ["CGO_ENABLED=0"]
  ldflags: ["-s", "-w"]
  version_var: main.version    # stamped with the tag's version
  extra_files: [LICENSE, NOTICE]
components:
  - name: token                # tags: token/vX.Y.Z, builds ./cmd/token
  - name: example
```

A component that names only itself inherits everything. `relkit validate` fails if a command
exists under `cmd/` that no component declares, so a new binary cannot ship unversioned and
unsigned by accident; the only ways past the gate (declare it, or list it in `ignored_cmds`)
are both visible in review.

### What a release produces

Under the tag `token/v0.1.0`, a GitHub release holding, for each platform:

```
token_v0.1.0_linux_amd64.tar.gz     ... one archive per os/arch (zip on Windows)
token_v0.1.0_linux_amd64.tar.gz.sbom.json
checksums.txt                       ... every artifact above, by sha256
checksums.txt.sig  checksums.txt.pem
provenance.intoto.jsonl
```

Archives are **reproducible**: `-trimpath`, `CGO_ENABLED=0`, entries sorted, ownership zeroed,
and every timestamp taken from the commit rather than the clock. Two builds of the same commit
are byte-identical, which is what makes the signature worth anything: anyone can rebuild the
tag and confirm the digests it covers are the ones this source produces.

`checksums.txt` is the only file signed. It commits to every other artifact by digest, so a
consumer verifies one keyless cosign signature and then checks whatever they downloaded
against it. The release notes carry the exact `cosign verify-blob` command, with the signing
identity read back out of the certificate Sigstore issued (not guessed from the tag: a release
run through a *reusable* workflow is bound to that workflow's identity, not the caller's).

### Using it

```yaml
# .github/workflows/release.yml, in the consuming repo
name: Release
on:
  push:
    tags: ["*/v*"]        # <component>/vX.Y.Z
permissions:
  contents: read
jobs:
  release:
    uses: ionalpha/go-ci/.github/workflows/monorepo-release.yml@main
    with:
      ci-ref: main
```

Add the PR-side gate to the repo's CI too, so the manifest is checked before a tag can trust it:

```yaml
  manifest:
    uses: ionalpha/go-ci/.github/workflows/monorepo-check.yml@main
    with:
      ci-ref: main
```

Locally, the same tool answers the same questions without touching anything:

```sh
relkit validate                     # the CI gate
relkit changed --only-changed       # which components have unreleased work
relkit plan   --tag token/v0.1.0    # everything the release would do, as JSON
relkit notes  --tag token/v0.1.0    # the release notes it would publish
relkit build  --tag token/v0.1.0    # the exact artifacts, into ./dist
```

`relkit release` refuses to run against a dirty tree, a tag that does not exist, a tag that
does not point at the commit being built, or a release that already exists. A published tag is
immutable in every consumer's mind, so overwriting its assets is never the right move.

## Pinning

For a supply-chain-tight setup, pin the `uses:` line to a tag or commit SHA (not `@main`) and
set `ci-ref` to the same ref, so a change here is adopted deliberately, never silently. The
actions inside this workflow are already pinned to commit SHAs and kept current by Dependabot
in the consuming repos.

## Repo-specific rules

This config holds only rules that apply to **every** ionalpha Go module. A repo with its own
architecture boundaries (layer direction, egress/inbound gates, determinism forbids) layers
those on top in its own tree; they do not belong here.
