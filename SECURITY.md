# Security Policy

## Reporting a vulnerability

Please report security issues **privately**. Do not open a public issue or pull request.

- Use GitHub's [private vulnerability reporting](https://github.com/ionalpha/go-ci/security/advisories/new)
  ("Report a vulnerability" under the Security tab), or
- email **contact@ionalpha.io**.

## Why this repo matters for security

This repository defines the shared CI that every ionalpha Go repo runs, so a change here
affects the integrity of many pipelines. Of particular interest: anything that could weaken a
gate silently (a lint/scan disabled or scoped away), an unpinned or maliciously-bumped action,
or a reusable-workflow change that lets a consumer's untrusted input run with elevated
permissions. Actions are pinned to commit SHAs and consumers should pin this repo's reusable
workflow to a tag or SHA.
