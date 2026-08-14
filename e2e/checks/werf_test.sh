#!/usr/bin/env bash

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=e2e/checks/werf.sh
source "$ROOT/e2e/checks/werf.sh"

assert_equals() {
  local expected="$1"
  local actual="$2"

  if [[ "$actual" != "$expected" ]]; then
    printf 'got %q, want %q\n' "$actual" "$expected" >&2
    exit 1
  fi
}

first_branch="$(werf_dev_branch /tmp/kubeconfig-proxy-worktree-a /tmp/werf-home-a)"
same_branch="$(werf_dev_branch /tmp/kubeconfig-proxy-worktree-a /tmp/werf-home-a)"
other_branch="$(werf_dev_branch /tmp/kubeconfig-proxy-worktree-b /tmp/werf-home-a)"
other_home_branch="$(werf_dev_branch /tmp/kubeconfig-proxy-worktree-a /tmp/werf-home-b)"

assert_equals "$first_branch" "$same_branch"
if [[ "$first_branch" == "$other_branch" ]]; then
  printf 'werf dev branches for distinct worktrees must differ\n' >&2
  exit 1
fi
if [[ "$first_branch" == "$other_home_branch" ]]; then
  printf 'werf dev branches for distinct cache homes must differ\n' >&2
  exit 1
fi
if [[ ! "$first_branch" =~ ^_werf-dev-kcp-e2e-[0-9]+$ ]]; then
  printf 'unexpected werf dev branch %q\n' "$first_branch" >&2
  exit 1
fi

declare -a commands=()

run_cmd() {
  commands+=("$4")
}

expect_exists() { :; }
expect_not_found() { :; }
expect_namespace_deleted_or_terminating() { :; }
e2e_resource_name() { printf 'test-%s' "$1"; }

KUBECONFIG_FILE=/tmp/kubeconfig
WERF_NS=test-namespace
PROXY_CONTEXT=test-proxy
WERF_TIMEOUT=180
E2E_RESOURCE_PREFIX=test-prefix
CTX_A=context-a
CTX_B=context-b
KCP_SKIP_WERF=0

run_werf_checks

expected_branch="$(werf_dev_branch "$ROOT")"
if [[ "${commands[0]}" != *"--dev-branch '$expected_branch'"* ]]; then
  printf 'werf converge command does not use isolated dev branch %q:\n%s\n' "$expected_branch" "${commands[0]}" >&2
  exit 1
fi
