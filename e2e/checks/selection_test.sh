#!/usr/bin/env bash

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=e2e/checks/selection.sh
source "$ROOT/e2e/checks/selection.sh"

assert_enabled() {
  local selected="$1"
  local check="$2"
  KCP_E2E_CHECKS="$selected"
  parse_selected_checks
  if ! is_check_selected "$check"; then
    printf 'expected %s to select %s\n' "$selected" "$check" >&2
    exit 1
  fi
}

assert_disabled() {
  local selected="$1"
  local check="$2"
  KCP_E2E_CHECKS="$selected"
  parse_selected_checks
  if is_check_selected "$check"; then
    printf 'expected %s not to select %s\n' "$selected" "$check" >&2
    exit 1
  fi
}

assert_enabled all context
assert_enabled all werf
assert_enabled ' all ' context
assert_enabled 'routing, watch' routing
assert_enabled 'routing, watch' watch
assert_disabled 'routing, watch' aggregation

assert_rejected() {
  local selected="$1"
  KCP_E2E_CHECKS="$selected"
  if parse_selected_checks >/dev/null 2>&1; then
    printf 'expected %s to be rejected\n' "$selected" >&2
    exit 1
  fi
}

assert_rejected 'routing,unknown'
assert_rejected 'all,routing'
assert_rejected 'routing,'
assert_rejected 'sub resources'
