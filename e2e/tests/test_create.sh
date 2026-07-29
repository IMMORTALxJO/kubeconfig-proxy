#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/_libs.sh"

r="$(test_name create)"

cleanup() {
    local status=$?

    delete_test_namespace "$r"
    cleanup_test_file
    exit "$status"
}
trap cleanup EXIT INT TERM

create_test_namespace "$r"


k_a create cm "${r}" --from-literal=key1=value1 -n "${r}"
e_a cm/"${r}" "${r}"
e_p cm/"${r}" "${r}"
ne_b cm/"${r}" "${r}"

k_b create cm "${r}" --from-literal=key1=value1 -n "${r}"
e_b cm/"${r}" "${r}"
e_p cm/"${r}" "${r}"

k_p get cm -L context -n "${r}" | tee "${T}"

s "${r}.*${CONTEXT_A}"
s "${r}.*${CONTEXT_B}"

k_p delete cm "${r}" -n "${r}"
ne_p cm/"${r}" "${r}"
