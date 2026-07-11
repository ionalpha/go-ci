# Contributing

go-ci holds the **shared** CI for ionalpha Go repositories: the reusable
`go-ci` workflow and the golangci-lint / gitleaks config every consumer runs. A change here
changes the gate for `flynn`, `flynn-extensions`, and future repos at once, so treat it with
care.

## Ground rules

1. **One PR, one topic**, and reference an issue for anything non-trivial.
2. **Never weaken a gate silently.** Removing or scoping down a linter, a scanner, or a test
   pass must be called out explicitly and justified in the PR.
3. **Pin everything.** Actions are pinned to full commit SHAs (Dependabot bumps them).
   Tool versions (golangci-lint, gitleaks, govulncheck) are pinned too.
4. **Prove it against a consumer.** Before changing the workflow, run it against a real
   consumer branch (open a draft PR in `flynn-extensions` that points its caller at your
   branch: `uses: ionalpha/go-ci/.github/workflows/go-ci.yml@<your-branch>`), and confirm
   it is green.
5. **Sign off with DCO:** `git commit -s`, and use Conventional Commits.

## Layout

- `.github/workflows/go-ci.yml` - the reusable workflow (`workflow_call`).
- `.golangci.yml` / `.gitleaks.toml` - shared config, checked out by the workflow at run time.

See [`README.md`](README.md) for how a repo consumes this.
