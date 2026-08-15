#!/usr/bin/env bash

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=e2e/update-compatibility-profiles.sh
source "$ROOT/e2e/update-compatibility-profiles.sh"

assert_equals() {
	local actual="$1"
	local expected="$2"

	if [[ "$actual" != "$expected" ]]; then
		printf 'got %s, want %s\n' "$actual" "$expected" >&2
		exit 1
	fi
}

assert_profiles() {
	local stable_version="$1"
	shift
	local -a expected=("$@")
	local -a actual=()

	while IFS= read -r profile; do
		actual+=("$profile")
	done < <(kcp_compatibility_profiles_for "$stable_version")
	if [[ "${actual[*]}" != "${expected[*]}" ]]; then
		printf 'profiles for %s: got %s, want %s\n' "$stable_version" "${actual[*]}" "${expected[*]}" >&2
		exit 1
	fi
}

assert_profiles v1.36.2 1.34 1.35 1.36
assert_profiles v1.37.0 1.35 1.36 1.37

if kcp_compatibility_profiles_for v2.1.0 >/dev/null 2>&1; then
	printf 'non-1.x Kubernetes version was accepted\n' >&2
	exit 1
fi
if kcp_compatibility_profiles_for v1.2.0 >/dev/null 2>&1; then
	printf 'Kubernetes minor without three supported releases was accepted\n' >&2
	exit 1
fi

temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/kcp-compatibility-test.XXXXXX")"
trap 'rm -rf "$temporary_dir"' EXIT

tag_remote="$temporary_dir/kubernetes.git"
tag_worktree="$temporary_dir/kubernetes-worktree"
git init --bare "$tag_remote" >/dev/null
git init "$tag_worktree" >/dev/null
git -C "$tag_worktree" config user.name test
git -C "$tag_worktree" config user.email test@example.com
git -C "$tag_worktree" commit --allow-empty -m test >/dev/null
tag_commit="$(git -C "$tag_worktree" rev-parse HEAD)"
git -C "$tag_worktree" tag -a v1.99.0 -m v1.99.0
git -C "$tag_worktree" push "$tag_remote" v1.99.0 >/dev/null
assert_equals "$(resolve_kubernetes_tag_commit v1.99.0 "$tag_remote")" "$tag_commit"

WORK_DIR="$temporary_dir/profile-data"
mkdir -p "$WORK_DIR"
DESIRED_PROFILES=(1.34 1.35 1.36)

for profile in "${DESIRED_PROFILES[@]}"; do
	prepare_profile_data "$profile"
done
VERSIONS_FILE="$temporary_dir/versions.sh"
VERSIONS_TEST_FILE="$temporary_dir/versions_test.sh"
COMPATIBILITY_FILE="$temporary_dir/COMPATIBILITY.md"
WORKFLOW_FILE="$temporary_dir/compatibility.yml"
write_versions_file
write_versions_test_file
write_compatibility_workflow
write_compatibility_document

bash -n "$VERSIONS_FILE" "$VERSIONS_TEST_FILE"
# shellcheck source=/dev/null
source "$VERSIONS_FILE"
for profile in "${DESIRED_PROFILES[@]}"; do
	kcp_select_cluster_version "$profile"
	kcp_select_kubectl_version "$profile"
	assert_equals "$KCP_SELECTED_KUBECTL_VERSION" "${KCP_SELECTED_KUBERNETES_VERSION}"
done
line_continuation=$'\\'
rg -F "KCP_CLUSTER_B_VERSION_PROFILE=1.35 $line_continuation" "$COMPATIBILITY_FILE" >/dev/null
rg -F 'KCP_CLUSTER_A_VERSION_PROFILE: ${{ matrix.cluster }}' "$WORKFLOW_FILE" >/dev/null
rg -F 'KCP_CLUSTER_B_VERSION_PROFILE: ${{ matrix.cluster }}' "$WORKFLOW_FILE" >/dev/null
rg -F 'All 9 combinations run only when a maintainer starts the' "$COMPATIBILITY_FILE" >/dev/null
if rg -F 'matrix.primary' "$WORKFLOW_FILE" >/dev/null || rg -F 'matrix.secondary' "$WORKFLOW_FILE" >/dev/null; then
	printf 'generated workflow still contains a primary/secondary version matrix\n' >&2
	exit 1
fi
if rg -F 'upstream-kubectl:' "$WORKFLOW_FILE" >/dev/null; then
	printf 'generated compatibility workflow still contains upstream kubectl jobs\n' >&2
	exit 1
fi

(
	cp "$ROOT/e2e/versions.sh" "$temporary_dir/current-versions.sh"
	VERSIONS_FILE="$temporary_dir/current-versions.sh"
	VERSIONS_TEST_FILE="$temporary_dir/updated-versions_test.sh"
	COMPATIBILITY_FILE="$temporary_dir/updated-COMPATIBILITY.md"
	WORKFLOW_FILE="$temporary_dir/updated-compatibility.yml"
	resolve_new_profile() {
		printf '%s\t%s\t%s\n' 'v1.37.0' 'kindest/node:v1.37.0@sha256:test' 'test-commit'
	}
	KCP_COMPATIBILITY_STABLE_VERSION=v1.37.0 main

	# shellcheck source=/dev/null
	source "$VERSIONS_FILE"
	kcp_select_cluster_version 1.37
	assert_equals "$KCP_SELECTED_KUBERNETES_VERSION" v1.37.0
	assert_equals "$KCP_SELECTED_KUBERNETES_COMMIT" test-commit
	if kcp_select_cluster_version 1.34 >/dev/null 2>&1; then
		printf 'expired Kubernetes profile was retained\n' >&2
		exit 1
	fi
	rg -F 'cluster: ["1.35", "1.36", "1.37"]' "$WORKFLOW_FILE" >/dev/null
	rg -F 'repository is Kubernetes and kubectl `1.35, 1.36, 1.37`.' "$COMPATIBILITY_FILE" >/dev/null
)
