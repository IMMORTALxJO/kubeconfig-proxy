#!/usr/bin/env bash

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=e2e/versions.sh
source "$ROOT/e2e/versions.sh"

assert_equals() {
	local actual="$1"
	local expected="$2"

	if [[ "$actual" != "$expected" ]]; then
		printf 'got %s, want %s\n' "$actual" "$expected" >&2
		exit 1
	fi
}

kcp_select_cluster_version 1.34
assert_equals "$KCP_SELECTED_KUBERNETES_VERSION" "v1.34.0"
assert_equals "$KCP_SELECTED_KIND_NODE_IMAGE" "kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"

kcp_select_cluster_version 1.35
assert_equals "$KCP_SELECTED_KUBERNETES_VERSION" "v1.35.0"
assert_equals "$KCP_SELECTED_KUBERNETES_COMMIT" "2049416c7235eeec9a413c38472708e49af3ed88"

kcp_select_cluster_version 1.36
assert_equals "$KCP_SELECTED_KUBERNETES_VERSION" "v1.36.1"
assert_equals "$KCP_SELECTED_KUBERNETES_COMMIT" "756939600b9a7180fc2df6550a4585b638875e67"

kcp_select_kubectl_version 1.34
assert_equals "$KCP_SELECTED_KUBECTL_VERSION" "v1.34.0"
kcp_select_kubectl_version 1.35
assert_equals "$KCP_SELECTED_KUBECTL_VERSION" "v1.35.0"
kcp_select_kubectl_version 1.36
assert_equals "$KCP_SELECTED_KUBECTL_VERSION" "v1.36.1"

if kcp_select_cluster_version 1.33 >/dev/null 2>&1; then
	printf 'unsupported cluster profile was accepted\n' >&2
	exit 1
fi
if kcp_select_kubectl_version 1.37 >/dev/null 2>&1; then
	printf 'unsupported kubectl profile was accepted\n' >&2
	exit 1
fi
