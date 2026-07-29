#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

run_werf_checks() {
  local helm_output
  local helm_count

  run_cmd "seed helm release storage in both clusters" bash -c "
    set -euo pipefail
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create secret generic sh.helm.release.v1.kcp.v1 --from-literal=release=a --dry-run=client -o yaml | KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' apply -f -
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' label secret sh.helm.release.v1.kcp.v1 owner=helm name=kcp --overwrite
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create secret generic sh.helm.release.v1.kcp.v1 --from-literal=release=b --dry-run=client -o yaml | KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' apply -f -
    KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' label secret sh.helm.release.v1.kcp.v1 owner=helm name=kcp --overwrite
  "
  helm_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get secrets -l owner=helm,name=kcp -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>&1)"
  helm_count="$(printf '%s\n' "$helm_output" | sed '/^$/d' | wc -l | tr -d ' ')"
  if [[ "$helm_count" == "1" && "$helm_output" == *"sh.helm.release.v1.kcp.v1"* ]]; then
    add_result "PASS" "helm-release-proxy reads release storage from primary only" "one release storage item returned"
  else
    add_result "FAIL" "helm-release-proxy reads release storage from primary only" "$helm_output"
  fi

  if [[ "${KCP_SKIP_WERF:-0}" == "1" ]]; then
    add_result "SKIP" "werf example converge and dismiss" "KCP_SKIP_WERF=1"
    return
  fi

  run_cmd "werf example converge" bash -c "
    set -euo pipefail
    cd '$ROOT/examples/werf'
    KUBECONFIG='$KUBECONFIG_FILE' werf converge --env kind --dev --namespace '$WERF_NS' --kube-context '$PROXY_CONTEXT' --timeout '$WERF_TIMEOUT'
  "
  expect_exists "werf nginx deployment exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$WERF_NS" get deployment kubeconfig-proxy-werf-nginx
  expect_exists "werf nginx deployment exists in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$WERF_NS" get deployment kubeconfig-proxy-werf-nginx
  expect_exists "werf single-context job exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$WERF_NS" get job kubeconfig-proxy-werf-smoke
  expect_not_found "werf single-context job absent from kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$WERF_NS" get job kubeconfig-proxy-werf-smoke

  run_cmd "werf example dismiss" bash -c "
    set -euo pipefail
    cd '$ROOT/examples/werf'
    KUBECONFIG='$KUBECONFIG_FILE' werf dismiss --env kind --with-namespace --namespace '$WERF_NS' --kube-context '$PROXY_CONTEXT'
  "
  expect_namespace_deleted_or_terminating "werf namespace removed from kubeconfig-proxy-a" "$CTX_A"
  expect_namespace_deleted_or_terminating "werf namespace removed from kubeconfig-proxy-b" "$CTX_B"
}
