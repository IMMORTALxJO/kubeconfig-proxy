---
name: pr-review
description: Create or continue a GitHub pull request, address its actionable review feedback and CI failures, and monitor it until no actionable comments or unresolved review threads remain and required CI checks are green. Use when the user asks to create a PR, work through PR review comments, repair a failing PR build, or babysit a PR through review and CI. Do not use to merge a PR unless the user explicitly asks.
---

# PR Review Loop

Use the GitHub CLI (`gh`) against the repository in the current worktree. Do
not install a GitHub plugin when `gh` is available.

## Preconditions

1. Confirm the repository, current branch, remotes, and `gh auth status`.
2. Refuse to use the default branch as the PR head. Create a `codex/` branch if
   needed.
3. Inspect `git status` and `git diff`. Preserve unrelated user changes; do not
   stage or commit them without explicit approval.
4. Run the smallest relevant local tests before opening or updating the PR.

If an open PR already exists for the branch, continue with it. Otherwise, make
one focused commit from the scoped changes, push the branch, and create the PR:

```bash
gh pr create --fill
```

Use a clear title and body when `--fill` would not describe the change. Do not
merge, close, or force-push the PR unless the user explicitly asks.

## Review and CI cycle

Repeat this cycle until the completion criteria are met:

1. Record the PR number and current head SHA with `gh pr view`.
2. Fetch every feedback surface: unresolved review threads, submitted reviews,
   inline review comments, and general PR conversation comments. Use
   `gh api graphql --paginate` for review threads when there may be more than
   one page; `gh pr view --comments` alone is not sufficient because it does
   not show thread resolution state.
3. Classify feedback. Address explicit change requests, credible bug reports,
   unanswered technical questions, and CI-failure reports. Treat bot status
   notifications, acknowledgements, and duplicate comments as non-actionable.
   Never dismiss a concern merely to make the PR look clean.
4. Make the smallest safe fix for each actionable item. Follow repository
   instructions, add or update focused tests first, run the relevant tests, and
   perform a code review before committing.
5. Reply in each addressed conversation with the fix and the validating test.
   Resolve an inline review thread only after its concern is addressed; leave it
   unresolved when it needs reviewer or user input.
6. Commit only the intended changes, push the branch, then refresh comments and
   CI. New commits may create new reviews or checks, so never rely on a stale
   snapshot.
7. Wait for required checks with `gh pr checks <pr> --required`. When checks
   fail, inspect the corresponding run with `gh run view <run-id> --log-failed`,
   fix the failure, test, commit, push, and return to step 2. Do not treat a
   pending, cancelled, or failed required check as green.

When waiting for people or CI, use the app's recurring monitor when available.
Otherwise poll GitHub and yield progress at intervals no longer than 60 seconds;
do not use one long blocking wait. Keep monitoring after a green run briefly to
catch newly posted review feedback.

## Completion criteria

Finish only after a fresh GitHub snapshot shows all of the following for the
current head SHA:

- Every actionable comment has a substantive response and its fix is pushed.
- No actionable unresolved review thread remains.
- There is no outstanding `CHANGES_REQUESTED` review that needs a response.
- All required CI checks have passed (or are explicitly skipped by the workflow);
  none are pending, failed, or cancelled.

If a reviewer response, a repository permission, an external outage, or a
product decision is the only remaining blocker, report that exact blocker and
continue monitoring when the user asked to babysit the PR. Do not fabricate an
approval, resolve an unaddressed thread, or merge the PR.

## Handoff

Report the PR URL, branch, commits pushed, feedback addressed, latest required
check status, and any remaining human or external blocker. Include the commands
and test results used to validate each fix.
