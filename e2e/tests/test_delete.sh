#!/bin/bash

set -euo pipefail
source "$(dirname "$0")/_libs.sh"

r="$(test_name delete)"
configmap="$(test_name delete-configmap)"

cleanup() {
    local status=$?

    delete_test_namespace "$r"
    cleanup_test_file
    exit "$status"
}
trap cleanup EXIT INT TERM

create_test_namespace "$r"
k_b -n "$r" create configmap "$configmap" --from-literal=value=delete-me
ne_a "configmap/$configmap" "$r"

k_p -n "$r" delete "configmap/$configmap"
ne_a "configmap/$configmap" "$r"
ne_b "configmap/$configmap" "$r"
