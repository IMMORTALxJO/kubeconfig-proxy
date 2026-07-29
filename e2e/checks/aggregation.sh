#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

run_aggregation_checks() {
  local aggregate_output
  local paginated_output
  local paginated_count
  local readonly_list_output

  run_cmd "seed aggregate resources in source clusters" bash -c "
    set -euo pipefail
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create configmap kcp-only-a --from-literal=value=a
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' label configmap kcp-only-a kcp-pagination=yes
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create configmap kcp-only-b --from-literal=value=b
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' label configmap kcp-only-b kcp-pagination=yes
  "

  aggregate_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
  if [[ "$aggregate_output" == *"kcp-only-a=$CTX_A"* && "$aggregate_output" == *"kcp-only-b=$CTX_B"* ]]; then
    add_result "PASS" "aggregated list adds context labels" "saw kcp-only-a=$CTX_A and kcp-only-b=$CTX_B"
  else
    add_result "FAIL" "aggregated list adds context labels" "$aggregate_output"
  fi

  paginated_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps -l kcp-pagination=yes --chunk-size=1 -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
  paginated_count="$(printf '%s\n' "$paginated_output" | sed '/^$/d' | wc -l | tr -d ' ')"
  if [[ "$paginated_count" == "2" && "$paginated_output" == *"kcp-only-a=$CTX_A"* && "$paginated_output" == *"kcp-only-b=$CTX_B"* ]]; then
    add_result "PASS" "aggregated list pagination crosses target boundary" "chunk-size=1 returned both contexts exactly once"
  else
    add_result "FAIL" "aggregated list pagination crosses target boundary" "$paginated_output"
  fi

  readonly_list_output="$(kubectl_ctx "$RO_PROXY_CONTEXT" -n "$NS" get configmaps -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
  if [[ "$readonly_list_output" == *"kcp-only-a=$CTX_A"* && "$readonly_list_output" == *"kcp-only-b=$CTX_B"* ]]; then
    add_result "PASS" "read-only proxy allows list reads" "saw kcp-only-a=$CTX_A and kcp-only-b=$CTX_B"
  else
    add_result "FAIL" "read-only proxy allows list reads" "$readonly_list_output"
  fi
}
