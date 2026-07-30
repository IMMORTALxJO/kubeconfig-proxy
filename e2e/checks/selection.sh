#!/usr/bin/env bash

# Sourced by e2e/run.sh before setup so invalid selections fail quickly.

declare -a SELECTED_E2E_CHECKS=()

trim_e2e_check_name() {
  local value="$1"

  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

is_known_e2e_check() {
  case "$1" in
    context | aggregation | routing | rollout | subresources | watch | helm | werf) return 0 ;;
    *) return 1 ;;
  esac
}

parse_selected_checks() {
  local raw_checks
  local raw_check
  local check
  local -a raw_checks_array

  raw_checks="$(trim_e2e_check_name "${KCP_E2E_CHECKS:-all}")"
  SELECTED_E2E_CHECKS=()
  if [[ "$raw_checks" == *, || "$raw_checks" == ,* || "$raw_checks" == *,,* ]]; then
    printf 'KCP_E2E_CHECKS contains an empty check name\n' >&2
    return 1
  fi
  if [[ "$raw_checks" == "all" ]]; then
    SELECTED_E2E_CHECKS=(all)
    return 0
  fi
  IFS=',' read -r -a raw_checks_array <<<"$raw_checks"
  for raw_check in "${raw_checks_array[@]}"; do
    check="$(trim_e2e_check_name "$raw_check")"
    if [[ -z "$check" ]]; then
      printf 'KCP_E2E_CHECKS contains an empty check name\n' >&2
      return 1
    fi
    if [[ "$check" == "all" ]]; then
      printf 'KCP_E2E_CHECKS=all cannot be combined with other checks\n' >&2
      return 1
    fi
    if ! is_known_e2e_check "$check"; then
      printf 'unknown KCP_E2E_CHECKS value %q; valid values: all, context, aggregation, routing, rollout, subresources, watch, helm, werf\n' "$check" >&2
      return 1
    fi
    SELECTED_E2E_CHECKS+=("$check")
  done
}

is_check_selected() {
  local wanted="$1"
  local selected

  for selected in "${SELECTED_E2E_CHECKS[@]}"; do
    if [[ "$selected" == "all" || "$selected" == "$wanted" ]]; then
      return 0
    fi
  done
  return 1
}
