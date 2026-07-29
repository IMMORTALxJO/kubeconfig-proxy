#!/bin/bash

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export ROOT

k_p() {
    "${KUBECTL_BIN}" --kubeconfig="$KUBECONFIG" --context="$CONTEXT_PROXY" "$@"
}

k_a() {
    "${KUBECTL_BIN}" --kubeconfig="$KUBECONFIG" --context="$CONTEXT_A" "$@"
}

k_b() {
    "${KUBECTL_BIN}" --kubeconfig="$KUBECONFIG" --context="$CONTEXT_B" "$@"
}

kcp() {
    "${KCP_BIN}" --kubeconfig="$KUBECONFIG" "$@"
}

TS=$(date +%s)
export TS
T=$(mktemp)
export T

# shellcheck disable=SC2153 # NAMESPACE is provided by e2e/run.sh.
test_name() {
    printf '%s-%s-%s' "$NAMESPACE" "$TS" "$1"
}

create_test_namespace() {
    local namespace=$1

    k_p create namespace "$namespace"
    e_a "namespace/$namespace"
    e_b "namespace/$namespace"
}

delete_test_namespace() {
    k_p delete namespace "$1" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

cleanup_test_file() {
    rm -f "$T"
}

assert_contains() {
    local value=$1
    local pattern=$2

    if [[ "$value" != *"$pattern"* ]]; then
        echo "Expected output to contain '$pattern', got: $value"
        return 1
    fi
}

assert_equal() {
    local actual=$1
    local expected=$2

    if [[ "$actual" != "$expected" ]]; then
        echo "Expected '$expected', got '$actual'"
        return 1
    fi
}

wait_for_pod_ready() {
    local command=$1
    local pod=$2
    local namespace=$3

    "$command" -n "$namespace" wait --for=condition=Ready "pod/$pod" --timeout=90s
}

# check if resource exists
e() {
    local cluster=$1
    local resource=$2
    local namespace=${3:-default}
    local cmd=""
    if [ "$cluster" = "A" ]; then
        cmd=k_a
    elif [ "$cluster" = "B" ]; then
        cmd=k_b
    elif [ "$cluster" = "P" ]; then
        cmd=k_p
    fi;
    if ! $cmd get "$resource" -n "$namespace" >/dev/null 2>&1; then
        echo "Resource $resource does not exist in cluster $cluster"
        return 1
    fi
    echo "Resource $resource exists in cluster $cluster"
}
e_a() {
    e "A" "$1" "${2:-default}"
}
e_b() {
    e "B" "$1" "${2:-default}"
}
e_p() {
    e "P" "$1" "${2:-default}"
}

# check if resource does not exist
ne() {
    if e "${1}" "${2}" "${3:-default}"; then
        echo "Resource $2 exists in cluster ${1}, but it should not"
        return 1
    fi
    echo "Resource $2 does not exist in cluster ${1}, as expected"
    return 0
}

ne_a() {
    ne "A" "$1" "${2:-default}"
}
ne_b() {
    ne "B" "$1" "${2:-default}"
}
ne_p() {
    ne "P" "$1" "${2:-default}"
}

# search string in tmp file ( $T )
s() {
    local pattern=$1
    if grep -q "$pattern" "${T}"; then
        echo "Pattern '$pattern' found in tmp file"
        return 0
    else
        echo "Pattern '$pattern' not found in tmp file"
        return 1
    fi
}
