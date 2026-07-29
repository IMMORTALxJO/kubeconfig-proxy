#!/bin/bash

set -euo pipefail
source "$(dirname "$0")/_libs.sh"

r="$(test_name werf)"

cleanup() {
    local status=$?

    (
        cd "$ROOT/examples/werf"
        KUBECONFIG="$KUBECONFIG" werf dismiss --env kind --with-namespace --namespace "$r" --kube-context "$CONTEXT_PROXY"
    ) >/dev/null 2>&1 || true
    k_a delete namespace "$r" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    k_b delete namespace "$r" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    cleanup_test_file
    exit "$status"
}
trap cleanup EXIT INT TERM

command -v werf >/dev/null
(
    cd "$ROOT/examples/werf"
    KUBECONFIG="$KUBECONFIG" werf converge --env kind --dev --namespace "$r" --kube-context "$CONTEXT_PROXY" --timeout "${KCP_WERF_TIMEOUT:-180}"
)

e_a deployment/kubeconfig-proxy-werf-nginx "$r"
e_b deployment/kubeconfig-proxy-werf-nginx "$r"
e_a job/kubeconfig-proxy-werf-smoke "$r"
ne_b job/kubeconfig-proxy-werf-smoke "$r"

(
    cd "$ROOT/examples/werf"
    KUBECONFIG="$KUBECONFIG" werf dismiss --env kind --with-namespace --namespace "$r" --kube-context "$CONTEXT_PROXY"
)
ne_a deployment/kubeconfig-proxy-werf-nginx "$r"
ne_b deployment/kubeconfig-proxy-werf-nginx "$r"
