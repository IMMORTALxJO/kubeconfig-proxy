#!/bin/bash

set -euo pipefail
source "$(dirname "$0")/_libs.sh"

r="$(test_name patch)"
configmap="$(test_name patch-configmap)"

cleanup() {
    local status=$?

    delete_test_namespace "$r"
    cleanup_test_file
    exit "$status"
}
trap cleanup EXIT INT TERM

create_test_namespace "$r"
k_b -n "$r" create configmap "$configmap" --from-literal=value=before
ne_a "configmap/$configmap" "$r"

k_p -n "$r" patch "configmap/$configmap" --type merge -p '{"data":{"value":"patched"}}'
assert_equal "$(k_b -n "$r" get "configmap/$configmap" -o jsonpath='{.data.value}')" "patched"
ne_a "configmap/$configmap" "$r"
