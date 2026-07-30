#!/usr/bin/env bash

# Sourced by e2e/run.sh before any Kubernetes resources are created.

e2e_branch_prefix() {
  local branch="$1"
  local slug
  local checksum

  slug="$(printf '%s' "$branch" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-')"
  slug="${slug#-}"
  slug="${slug%-}"
  if [[ -z "$slug" ]]; then
    slug="local"
  fi
  slug="${slug:0:15}"
  slug="${slug%-}"
  checksum="$(printf '%s' "$branch" | cksum | awk '{print $1}')"
  printf 'kcp-e2e-%s-%s' "$slug" "${checksum:0:8}"
}

configure_e2e_prefix() {
  local branch

  branch="${KCP_E2E_BRANCH:-$(git branch --show-current 2>/dev/null)}"
  E2E_RESOURCE_PREFIX="${KCP_E2E_PREFIX:-$(e2e_branch_prefix "$branch")}"
  if [[ ! "$E2E_RESOURCE_PREFIX" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || [[ ${#E2E_RESOURCE_PREFIX} -gt 32 ]]; then
    printf 'KCP_E2E_PREFIX must be a lowercase DNS label no longer than 32 characters\n' >&2
    return 1
  fi
  # shellcheck disable=SC2034 # Used by e2e check files sourced by the runner.
  NS="$(e2e_resource_name namespace)"
  # shellcheck disable=SC2034 # Used by e2e check files sourced by the runner.
  AGGREGATION_TEST_SELECTOR="kubeconfig-proxy.io/e2e-run=$E2E_RESOURCE_PREFIX"
}

e2e_resource_name() {
  printf '%s-%s' "$E2E_RESOURCE_PREFIX" "$1"
}
