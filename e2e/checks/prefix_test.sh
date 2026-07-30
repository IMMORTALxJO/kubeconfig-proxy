#!/usr/bin/env bash

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=e2e/checks/prefix.sh
source "$ROOT/e2e/checks/prefix.sh"

assert_equals() {
  local expected="$1"
  local actual="$2"

  if [[ "$actual" != "$expected" ]]; then
    printf 'got %q, want %q\n' "$actual" "$expected" >&2
    exit 1
  fi
}

assert_matches() {
  local pattern="$1"
  local actual="$2"

  if [[ ! "$actual" =~ $pattern ]]; then
    printf 'got %q, want pattern %q\n' "$actual" "$pattern" >&2
    exit 1
  fi
}

feature_prefix="$(e2e_branch_prefix 'Feature/Add-E2E')"
assert_matches '^kcp-e2e-feature-add-e2e-[0-9]+$' "$feature_prefix"

slash_prefix="$(e2e_branch_prefix 'feature/foo')"
dash_prefix="$(e2e_branch_prefix 'feature-foo')"
if [[ "$slash_prefix" == "$dash_prefix" ]]; then
  printf 'sanitized branch names must still have distinct prefixes\n' >&2
  exit 1
fi

KCP_E2E_PREFIX='kcp-e2e-explicit'
KCP_E2E_BRANCH='ignored'
configure_e2e_prefix
assert_equals 'kcp-e2e-explicit' "$E2E_RESOURCE_PREFIX"
assert_equals 'kcp-e2e-explicit-namespace' "$NS"
assert_equals 'kcp-e2e-explicit-configmap' "$(e2e_resource_name configmap)"

KCP_E2E_PREFIX='kcp-e2e-12345678901234567890-1234'
if configure_e2e_prefix >/dev/null 2>&1; then
  printf 'expected an oversized prefix to be rejected\n' >&2
  exit 1
fi

KCP_E2E_PREFIX='INVALID'
if configure_e2e_prefix >/dev/null 2>&1; then
  printf 'expected invalid prefix to be rejected\n' >&2
  exit 1
fi
