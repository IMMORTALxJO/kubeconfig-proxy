#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/_libs.sh"

# shellcheck disable=SC2153 # NAMESPACE is provided by e2e/run.sh.
r="$(test_name watch)"
watch_pid=""

cleanup() {
    local status=$?

    if [[ -n "$watch_pid" ]]; then
        kill "$watch_pid" 2>/dev/null || true
        wait "$watch_pid" 2>/dev/null || true
    fi
    delete_test_namespace "$r"
    cleanup_test_file
    exit "$status"
}
trap cleanup EXIT INT TERM

create_test_namespace "$r"

"${KUBECTL_BIN}" --kubeconfig="$KUBECONFIG" --context="$CONTEXT_PROXY" get cm -w -n "${r}" >"${T}" 2>&1 &
watch_pid=$!

wait_for_pattern() {
    local pattern=$1
    local attempt

    for ((attempt = 0; attempt < 50; attempt++)); do
        if grep -q "$pattern" "$T"; then
            echo "Pattern '$pattern' found in watch output"
            return 0
        fi
        sleep 0.1
    done
    echo "Pattern '$pattern' not found in watch output"
    return 1
}

k_a create cm "${r}" --from-literal=key1=value1 -n "${r}"
wait_for_pattern "${r}"

k_b create cm "${r}-b" --from-literal=key1=value1 -n "${r}"
wait_for_pattern "${r}-b"
