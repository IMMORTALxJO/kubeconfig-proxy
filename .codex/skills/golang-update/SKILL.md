---
name: golang-update
description: Update a Go repository to the newest stable Go release and the newest compatible Go module versions. Use when the user asks to update Go, refresh Go dependencies, upgrade the Go toolchain, or perform regular Go dependency maintenance; when changes result, open and complete a pull request with pr-review-loop.
---

# Golang Update

## Workflow

1. Inspect `go.mod`, `go.sum`, the Makefile, CI/release workflows, version-pinned scripts, and the working tree. Preserve unrelated user changes.
2. Determine the newest stable Go release from the official Go releases endpoint or [go.dev/dl](https://go.dev/dl/). Do not select release candidates, betas, or release previews. Record the exact version.
3. Update every repository-owned Go toolchain pin that must stay aligned: the `go` directive, explicit `GOTOOLCHAIN` values, container-image tags, and CI setup that does not already read `go.mod`. Retain intentional compatibility exceptions only when documented; otherwise make the pins consistent.
4. With that toolchain, refresh module dependencies using Go tooling. Update direct dependencies deliberately (use `go get -u` with their module paths), then run `go mod tidy`. Keep the Kubernetes module family on a mutually compatible release line, and do not add dependencies merely to satisfy the update.
5. Review `go.mod` and `go.sum` for unintended additions, removals, downgrades, pre-release versions, or incompatible major-version jumps. Inspect the diff for other version references and update user-facing documentation if its stated Go requirement changed.
6. Run the smallest relevant tests first, then `GOTOOLCHAIN=auto go test ./...` and `make check`. Fix failures caused by the update; do not mask failures by lowering versions or weakening checks. Report unrelated pre-existing failures separately.

## Pull request

If the completed update leaves task-owned changes in the working tree, invoke `$pr-review-loop` and follow [its instructions](/Users/aleksandr.prusov/.codex/skills/pr-review-loop/SKILL.md): create or reuse a branch, commit the update, push it, open or update the PR, and continue until required checks pass and there is no actionable review feedback. Do not create an empty PR.

Before remote changes, confirm GitHub authentication and repository access as required by `pr-review-loop`. Treat invocation of this skill as authorization for the update and resulting PR workflow; do not merge the PR unless the user explicitly asks.

If no task-owned files changed after inspection and the update commands, report that the repository is current and do not start the PR workflow.

## Safety

Never use `go get -u ./...`, force-push, alter unrelated dependencies, or downgrade security-sensitive tooling to make checks pass. Do not disclose credentials, module-cache contents, kubeconfigs, or other secrets in output.
