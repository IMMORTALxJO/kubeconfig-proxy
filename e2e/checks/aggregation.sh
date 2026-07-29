#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

has_all_aggregation_items() {
  local output="$1"
  local i

  if [[ "$output" != *"kcp-only-a=$CTX_A"* || "$output" != *"kcp-only-b=$CTX_B"* ]]; then
    return 1
  fi
  for i in {1..9}; do
    if [[ "$output" != *"kcp-page-a-$i=$CTX_A"* || "$output" != *"kcp-page-b-$i=$CTX_B"* ]]; then
      return 1
    fi
  done
  return 0
}

run_aggregation_checks() {
  local aggregate_output
  local selector_output
  local selector_count
  local paginated_output
  local paginated_count
  local cleanup_a_output
  local cleanup_b_output
  local readonly_list_output

  run_cmd "prepare aggregate resources in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" delete configmap --selector="$AGGREGATION_TEST_SELECTOR" --ignore-not-found
  run_cmd "prepare aggregate resources in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" delete configmap --selector="$AGGREGATION_TEST_SELECTOR" --ignore-not-found

  run_cmd "seed aggregate resources in source clusters" bash -c "
    set -euo pipefail
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create configmap kcp-only-a --from-literal=value=a
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' label configmap kcp-only-a '$AGGREGATION_TEST_SELECTOR'
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create configmap kcp-only-b --from-literal=value=b
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' label configmap kcp-only-b '$AGGREGATION_TEST_SELECTOR'
    for i in {1..9}; do
      KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create configmap kcp-page-a-\$i --from-literal=value=a-\$i
      KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' label configmap kcp-page-a-\$i '$AGGREGATION_TEST_SELECTOR'
      KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create configmap kcp-page-b-\$i --from-literal=value=b-\$i
      KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' label configmap kcp-page-b-\$i '$AGGREGATION_TEST_SELECTOR'
    done
  "

  aggregate_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
  if [[ "$aggregate_output" == *"kcp-only-a=$CTX_A"* && "$aggregate_output" == *"kcp-only-b=$CTX_B"* ]]; then
    add_result "PASS" "aggregated list adds context labels" "saw kcp-only-a=$CTX_A and kcp-only-b=$CTX_B"
  else
    add_result "FAIL" "aggregated list adds context labels" "$aggregate_output"
  fi

  selector_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps --selector="$AGGREGATION_TEST_SELECTOR" -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
  selector_count="$(printf '%s\n' "$selector_output" | sed '/^$/d' | wc -l | tr -d ' ')"
  if [[ "$selector_count" == "20" ]] && has_all_aggregation_items "$selector_output"; then
    add_result "PASS" "aggregated selector list returns all test configmaps" "selector returned 20 items from both contexts"
  else
    add_result "FAIL" "aggregated selector list returns all test configmaps" "$selector_output"
  fi

  paginated_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps --selector="$AGGREGATION_TEST_SELECTOR" --chunk-size=1 -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
  paginated_count="$(printf '%s\n' "$paginated_output" | sed '/^$/d' | wc -l | tr -d ' ')"
  if [[ "$paginated_count" == "20" ]] && has_all_aggregation_items "$paginated_output"; then
    add_result "PASS" "aggregated list pagination crosses target boundary" "chunk-size=1 returned 20 items from both contexts exactly once"
  else
    add_result "FAIL" "aggregated list pagination crosses target boundary" "$paginated_output"
  fi

  readonly_list_output="$(kubectl_ctx "$RO_PROXY_CONTEXT" -n "$NS" get configmaps -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
  if [[ "$readonly_list_output" == *"kcp-only-a=$CTX_A"* && "$readonly_list_output" == *"kcp-only-b=$CTX_B"* ]]; then
    add_result "PASS" "read-only proxy allows list reads" "saw kcp-only-a=$CTX_A and kcp-only-b=$CTX_B"
  else
    add_result "FAIL" "read-only proxy allows list reads" "$readonly_list_output"
  fi

  run_cmd "delete aggregate resources through proxy selector" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" delete configmaps --selector="$AGGREGATION_TEST_SELECTOR"
  cleanup_a_output="$(kubectl_ctx "$CTX_A" -n "$NS" get configmaps --selector="$AGGREGATION_TEST_SELECTOR" -o jsonpath='{.items[*].metadata.name}' 2>&1)"
  cleanup_b_output="$(kubectl_ctx "$CTX_B" -n "$NS" get configmaps --selector="$AGGREGATION_TEST_SELECTOR" -o jsonpath='{.items[*].metadata.name}' 2>&1)"
  if [[ -z "$cleanup_a_output" && -z "$cleanup_b_output" ]]; then
    add_result "PASS" "selector delete removes aggregate test configmaps" "no matching configmaps remain in either source context"
  else
    add_result "FAIL" "selector delete removes aggregate test configmaps" "kubeconfig-proxy-a: $cleanup_a_output; kubeconfig-proxy-b: $cleanup_b_output"
  fi
}
