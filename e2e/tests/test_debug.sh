#!/bin/bash

set -euo pipefail
source "$(dirname "$0")/_libs.sh"

r="$(test_name debug)"
pod="$(test_name debug-pod)"
debug_container="debugger"

cleanup() {
    local status=$?

    delete_test_namespace "$r"
    cleanup_test_file
    exit "$status"
}
trap cleanup EXIT INT TERM

create_test_namespace "$r"
k_b -n "$r" run "$pod" --image=busybox:1.37 --restart=Never --command -- sh -c 'sleep 300'
wait_for_pod_ready k_b "$pod" "$r"
ne_a "pod/$pod" "$r"

if ! debug_output="$(k_p -n "$r" debug "pod/$pod" --image=busybox:1.37 --target="$pod" --container="$debug_container" --quiet 2>&1)"; then
    printf '%s\n' "$debug_output" >&2
    exit 1
fi
debug_name="$(k_b -n "$r" get "pod/$pod" -o jsonpath='{.spec.ephemeralContainers[?(@.name=="debugger")].name}')"
assert_equal "$debug_name" "$debug_container"
ne_a "pod/$pod" "$r"
