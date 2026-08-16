#!/usr/bin/env bash

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
VERSIONS_FILE="$ROOT/e2e/versions.sh"
VERSIONS_TEST_FILE="$ROOT/e2e/versions_test.sh"
COMPATIBILITY_FILE="$ROOT/COMPATIBILITY.md"
WORKFLOW_FILE="$ROOT/.github/workflows/compatibility.yml"
README_FILE="$ROOT/README.md"
WORK_DIR=""

kcp_compatibility_profiles_for() {
	local stable_version="$1"
	local minor

	if [[ ! "$stable_version" =~ ^v1\.([0-9]+)\.[0-9]+$ ]]; then
		printf 'unsupported Kubernetes stable version: %s\n' "$stable_version" >&2
		return 1
	fi
	minor="${BASH_REMATCH[1]}"
	if ((minor < 3)); then
		printf 'Kubernetes %s does not have three supported minor releases\n' "$stable_version" >&2
		return 1
	fi
	printf '1.%d\n1.%d\n1.%d\n' "$((minor - 2))" "$((minor - 1))" "$minor"
}

join_profiles() {
	local result=""
	local profile

	for profile in "$@"; do
		result="${result:+$result, }$profile"
	done
	printf '%s' "$result"
}

current_profiles() {
	awk '
		/^kcp_select_kubectl_version/ { exit }
		/^[[:space:]]*1[.][0-9]+\)/ {
			profile = $1
			sub(/\)/, "", profile)
			print profile
		}
	' "$VERSIONS_FILE"
}

profile_value() {
	local profile="$1"
	local field="$2"

	awk -v profile="$profile" -v field="$field" '
		$0 ~ "^[[:space:]]*" profile "\\)" { selected = 1; next }
		selected && $0 ~ "^[[:space:]]*" field "=" {
			value = $0
			sub("^[^=]*=\\\"", "", value)
			sub("\\\".*$", "", value)
			print value
			exit
		}
		selected && $0 ~ "^[[:space:]]*[0-9]+[.][0-9]+\\)" { exit }
	' "$VERSIONS_FILE"
}

resolve_kubernetes_tag_commit() {
	local release="$1"
	local remote="${2:-https://github.com/kubernetes/kubernetes.git}"
	local commit

	commit="$(git ls-remote "$remote" "refs/tags/$release^{}" | awk 'NR == 1 { print $1 }')"
	if [[ -z "$commit" ]]; then
		commit="$(git ls-remote --refs "$remote" "refs/tags/$release" | awk 'NR == 1 { print $1 }')"
	fi
	if [[ -z "$commit" ]]; then
		printf 'could not resolve Kubernetes tag %s\n' "$release" >&2
		return 1
	fi
	printf '%s\n' "$commit"
}

resolve_new_profile() {
	local profile="$1"
	local release="v${profile}.0"
	local commit
	local token
	local digest

	commit="$(resolve_kubernetes_tag_commit "$release")"
	token="$(curl -fsSL 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:kindest/node:pull' | jq -er '.token')"
	digest="$(curl -fsSI \
		-H "Authorization: Bearer $token" \
		-H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
		"https://registry-1.docker.io/v2/kindest/node/manifests/$release" |
		tr -d '\r' |
		awk 'tolower($1) == "docker-content-digest:" { print $2; exit }')"
	if [[ -z "$digest" ]]; then
		printf 'could not resolve kind node image digest for %s\n' "$release" >&2
		return 1
	fi
	printf '%s\t%s\t%s\n' "$release" "kindest/node:${release}@${digest}" "$commit"
}

prepare_profile_data() {
	local profile="$1"
	local release
	local image
	local commit

	if [[ -f "$WORK_DIR/$profile" ]]; then
		return
	fi
	release="$(profile_value "$profile" KCP_SELECTED_KUBERNETES_VERSION)"
	image="$(profile_value "$profile" KCP_SELECTED_KIND_NODE_IMAGE)"
	commit="$(profile_value "$profile" KCP_SELECTED_KUBERNETES_COMMIT)"
	if [[ -z "$release" || -z "$image" || -z "$commit" ]]; then
		IFS=$'\t' read -r release image commit < <(resolve_new_profile "$profile")
	fi
	printf '%s\t%s\t%s\n' "$release" "$image" "$commit" >"$WORK_DIR/$profile"
}

profile_field() {
	local profile="$1"
	local field="$2"
	awk -F '\t' -v field="$field" '{ print $field }' "$WORK_DIR/$profile"
}

# shellcheck disable=SC2016
write_versions_file() {
	local profile
	local release
	local image
	local commit

	{
		printf '%s\n' '#!/usr/bin/env bash'
		printf '\n%s\n' '# shellcheck disable=SC2034'
		printf '\n%s\n' '# Pinned versions used by the compatibility runners. A profile represents a'
		printf '%s\n' '# Kubernetes minor release; kind publishes a representative node image for the'
		printf '%s\n' '# minor rather than every upstream patch release.'
		printf '\n%s\n' 'kcp_select_cluster_version() {'
		printf '\t%s\n' 'local profile="$1"'
		printf '\n\t%s\n' 'case "$profile" in'
		for profile in "${DESIRED_PROFILES[@]}"; do
			release="$(profile_field "$profile" 1)"
			image="$(profile_field "$profile" 2)"
			commit="$(profile_field "$profile" 3)"
			printf '\t%s)\n' "$profile"
			printf '\t\tKCP_SELECTED_KUBERNETES_VERSION="%s"\n' "$release"
			printf '\t\tKCP_SELECTED_KIND_NODE_IMAGE="%s"\n' "$image"
			printf '\t\tKCP_SELECTED_KUBERNETES_COMMIT="%s"\n' "$commit"
			printf '\t\t;;\n'
		done
		printf '\t%s\n' '*)'
		printf '\t\t%s\n' "printf 'unsupported Kubernetes compatibility profile: %s\\n' \"\$profile\" >&2"
		printf '\t\t%s\n' 'return 1'
		printf '\t\t%s\n' ';;'
		printf '\tesac\n}\n'
		printf '\n%s\n' 'kcp_select_kubectl_version() {'
		printf '\t%s\n' 'local profile="$1"'
		printf '\n\t%s\n' 'case "$profile" in'
		for profile in "${DESIRED_PROFILES[@]}"; do
			release="$(profile_field "$profile" 1)"
			printf '\t%s) KCP_SELECTED_KUBECTL_VERSION="%s" ;;\n' "$profile" "$release"
		done
		printf '\t%s\n' '*)'
		printf '\t\t%s\n' "printf 'unsupported kubectl compatibility profile: %s\\n' \"\$profile\" >&2"
		printf '\t\t%s\n' 'return 1'
		printf '\t\t%s\n' ';;'
		printf '\tesac\n}\n'
	} >"$VERSIONS_FILE"
	chmod +x "$VERSIONS_FILE"
}

# shellcheck disable=SC2016
write_versions_test_file() {
	local profile
	local release
	local image
	local commit
	local unsupported_minor

	unsupported_minor="1.$((${DESIRED_PROFILES[0]#1.} - 1))"
	{
		printf '%s\n\n' '#!/usr/bin/env bash'
		printf '%s\n\n' 'set -euo pipefail'
		printf '%s\n' 'ROOT="$(git rev-parse --show-toplevel)"'
		printf '%s\n' '# shellcheck source=e2e/versions.sh'
		printf '%s\n\n' 'source "$ROOT/e2e/versions.sh"'
		printf '%s\n' 'assert_equals() {'
		printf '\t%s\n\t%s\n' 'local actual="$1"' 'local expected="$2"'
		printf '\n\t%s\n' 'if [[ "$actual" != "$expected" ]]; then'
		printf '\t\t%s\n\t\t%s\n\t\t%s\n\t%s\n' "printf 'got %s, want %s\\n' \"\$actual\" \"\$expected\" >&2" 'exit 1' 'fi' '}'
		for profile in "${DESIRED_PROFILES[@]}"; do
			release="$(profile_field "$profile" 1)"
			image="$(profile_field "$profile" 2)"
			commit="$(profile_field "$profile" 3)"
			printf '\nkcp_select_cluster_version %s\n' "$profile"
			printf 'assert_equals "$KCP_SELECTED_KUBERNETES_VERSION" "%s"\n' "$release"
			printf 'assert_equals "$KCP_SELECTED_KIND_NODE_IMAGE" "%s"\n' "$image"
			printf 'assert_equals "$KCP_SELECTED_KUBERNETES_COMMIT" "%s"\n' "$commit"
			printf 'kcp_select_kubectl_version %s\n' "$profile"
			printf 'assert_equals "$KCP_SELECTED_KUBECTL_VERSION" "%s"\n' "$release"
		done
		printf '\nif kcp_select_cluster_version %s >/dev/null 2>&1; then\n' "$unsupported_minor"
		printf '\t%s\n\t%s\n%s\n' "printf 'unsupported cluster profile was accepted\\n' >&2" 'exit 1' 'fi'
		printf 'if kcp_select_kubectl_version 1.99 >/dev/null 2>&1; then\n'
		printf '\t%s\n\t%s\n%s\n' "printf 'unsupported kubectl profile was accepted\\n' >&2" 'exit 1' 'fi'
	} >"$VERSIONS_TEST_FILE"
	chmod +x "$VERSIONS_TEST_FILE"
}

# shellcheck disable=SC2016
write_compatibility_workflow() {
	local profile
	local last_profile="${DESIRED_PROFILES[$((${#DESIRED_PROFILES[@]} - 1))]}"

	{
		printf '%s\n\n' 'name: Kubernetes compatibility'
		printf '%s\n\n' 'on: workflow_dispatch'
		printf '%s\n\n' 'permissions:' '  contents: read'
		printf '%s\n' 'jobs:'
		printf '%s\n' '  routing:'
		printf '%s\n' '    name: kubectl ${{ matrix.kubectl }}, Kubernetes ${{ matrix.cluster }}'
		printf '%s\n' '    runs-on: ubuntu-latest' '    timeout-minutes: 45' '    strategy:' '      fail-fast: false' '      matrix:'
		printf '        kubectl: ['
		for profile in "${DESIRED_PROFILES[@]}"; do printf '"%s"%s' "$profile" "$( [[ "$profile" == "$last_profile" ]] || printf ', ')"; done
		printf ']\n        cluster: ['
		for profile in "${DESIRED_PROFILES[@]}"; do printf '"%s"%s' "$profile" "$( [[ "$profile" == "$last_profile" ]] || printf ', ')"; done
		printf ']\n'
		printf '\n%s\n' '    steps:'
		printf '%s\n' '      - name: Checkout' '        uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1'
		printf '\n%s\n' '      - name: Set up Go' '        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0' '        with:' '          go-version-file: go.mod' '          cache: true'
		printf '\n%s\n' '      - name: Install kind' '        run: |' '          go install sigs.k8s.io/kind@v0.30.0' '          echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"'
		printf '\n%s\n' '      - name: Run routing compatibility suite' '        env:' '          KCP_SKIP_MAKE_CHECK: "1"' '          KCP_SKIP_WERF: "1"' '          KCP_E2E_CHECKS: context,aggregation,routing,rollout,subresources,watch,helm' '          KCP_E2E_PREFIX: compat' '          KCP_KUBECTL_VERSION_PROFILE: ${{ matrix.kubectl }}' '          KCP_CLUSTER_A_VERSION_PROFILE: ${{ matrix.cluster }}' '          KCP_CLUSTER_B_VERSION_PROFILE: ${{ matrix.cluster }}' '        run: e2e/run.sh'
	} >"$WORKFLOW_FILE"
}

# shellcheck disable=SC2016,SC1003
write_compatibility_document() {
	local profile
	local release
	local kubectl_profile
	local all_profiles
	local total_cells=$((${#DESIRED_PROFILES[@]} * ${#DESIRED_PROFILES[@]}))

	all_profiles="$(join_profiles "${DESIRED_PROFILES[@]}")"
	{
		printf '%s\n\n' '# Kubernetes Compatibility'
		printf '%s\n' '`kubeconfig-proxy` maintains a moving compatibility window for the Kubernetes' 'minor releases that are actively supported upstream. The window in this' "repository is Kubernetes and kubectl \`${all_profiles}\`."
		printf '\n%s\n' 'The exact binaries and node images are pinned in `e2e/versions.sh`. The kind' 'project does not publish a node image for every Kubernetes patch release, so a' 'profile validates the API minor using the listed representative patch version.' 'The profile names, not untested patch releases, are the compatibility contract.' 'The current result is the latest run of the [Kubernetes compatibility' 'workflow](https://github.com/IMMORTALxJO/kubeconfig-proxy/actions/workflows/compatibility.yml).'
		printf '\n%s\n' '| Profile | API server / kind node | kubectl | Kubernetes source tag |' '| --- | --- | --- | --- |'
		for profile in "${DESIRED_PROFILES[@]}"; do
			release="$(profile_field "$profile" 1)"
			printf '| %s | %s | %s | %s |\n' "$profile" "$release" "$release" "$release"
		done
		printf '\n%s\n' '## Compatibility Matrix'
		printf '\n%s\n' 'The `Kubernetes compatibility` workflow runs every kubectl and Kubernetes' 'profile pairing below. Both kind clusters use the same Kubernetes profile in' 'each cell while still exercising multi-context routing and aggregation.'
		printf '\n%s\n' '| kubectl | Kubernetes / kind clusters | Cells |' '| --- | --- | --- |'
		for kubectl_profile in "${DESIRED_PROFILES[@]}"; do
			printf '| %s | %s | %s |\n' "$kubectl_profile" "$all_profiles" "${#DESIRED_PROFILES[@]}"
		done
		printf '\nAll %s combinations run only when a maintainer starts the\n' "$total_cells"
		printf '%s\n' '`Kubernetes compatibility` workflow manually. It is not a required merge' 'check.'
		printf '\n%s\n' '## Supported Scope'
		printf '\n%s\n' 'The matrix verifies the routing contract for Kubernetes stable APIs: discovery,' 'CRUD and server-side mutations, lists and pagination, watches, pod connection' 'subresources, read-only contexts, Helm release storage, and source markers.' 'It does not claim feature equivalence for alpha/beta APIs, arbitrary CRDs,' 'aggregated API implementations, kubectl plugins, or provider-specific auth' 'plugins.'
		printf '\n%s\n' 'Kubernetes supports kubectl within one minor version of a kube-apiserver. The' 'matrix also runs the two edge pairings with a two-minor skew as compatibility' 'probes; they do not expand the upstream-supported skew policy. `client-go`' "\`v0.36\` remains the proxy's upstream client and is tested against all three" 'cluster profiles. See the upstream [version skew policy](https://kubernetes.io/releases/version-skew-policy/)' 'and [client-go compatibility matrix](https://github.com/kubernetes/client-go).'
		printf '\n%s\n' '## Running a Cell Locally'
		printf '\n%s\n' 'Use the profile variables to run an individual matrix cell. Recreate local kind' 'clusters when changing either cluster profile:'
		printf '\n%s\n' '```bash'
		printf '%s %s\n' 'KCP_RECREATE_KIND=1' '\\'
		printf '%s %s\n' "KCP_KUBECTL_VERSION_PROFILE=${DESIRED_PROFILES[0]}" '\\'
		printf '%s %s\n' "KCP_CLUSTER_A_VERSION_PROFILE=${DESIRED_PROFILES[1]}" '\\'
		printf '%s %s\n' "KCP_CLUSTER_B_VERSION_PROFILE=${DESIRED_PROFILES[1]}" '\\'
		printf '%s %s\n' 'KCP_SKIP_WERF=1' '\\'
		printf '%s\n' 'e2e/run.sh' '```'
		printf '\n%s\n' 'The upstream kubectl client suite is not part of the compatibility workflow.' 'To run it separately for a profile:'
		printf '\n%s\n' '```bash' "KCP_KUBERNETES_VERSION_PROFILE=${DESIRED_PROFILES[1]} e2e/run-upstream-kubectl-e2e.sh" '```'
		printf '\n%s\n' 'This file, `e2e/versions.sh`, its focused test, and the workflow matrix are' 'generated by `e2e/update-compatibility-profiles.sh`. The weekly refresh workflow' 'opens a pull request only when Kubernetes support profiles change.'
	} >"$COMPATIBILITY_FILE"
}

write_readme_compatibility_badge() {
	local badge_profiles=""
	local profile
	local replacement
	local temporary_file

	for profile in "${DESIRED_PROFILES[@]}"; do
		badge_profiles="${badge_profiles:+${badge_profiles}%20%7C%20}${profile}"
	done
	replacement="[![Kubernetes compatibility: $(join_profiles "${DESIRED_PROFILES[@]}")](https://img.shields.io/github/actions/workflow/status/IMMORTALxJO/kubeconfig-proxy/compatibility.yml?branch=master&label=Kubernetes%20${badge_profiles}&logo=kubernetes)](https://github.com/IMMORTALxJO/kubeconfig-proxy/actions/workflows/compatibility.yml)"
	temporary_file="$(mktemp "${README_FILE}.XXXXXX")"
	if ! awk -v replacement="$replacement" '
		/^\[!\[Kubernetes compatibility/ {
			print replacement
			replacements++
			next
		}
		{ print }
		END { if (replacements != 1) exit 1 }
	' "$README_FILE" >"$temporary_file"; then
		rm -f "$temporary_file"
		printf 'expected exactly one Kubernetes compatibility badge in %s\n' "$README_FILE" >&2
		return 1
	fi
	cp "$temporary_file" "$README_FILE"
	rm -f "$temporary_file"
}

main() {
	local stable_version
	local current_profile_list

	stable_version="${KCP_COMPATIBILITY_STABLE_VERSION:-$(curl -fsSL https://dl.k8s.io/release/stable.txt)}"
	DESIRED_PROFILES=()
	while IFS= read -r profile; do
		DESIRED_PROFILES+=("$profile")
	done < <(kcp_compatibility_profiles_for "$stable_version")
	CURRENT_PROFILES=()
	while IFS= read -r profile; do
		CURRENT_PROFILES+=("$profile")
	done < <(current_profiles)
	current_profile_list="${CURRENT_PROFILES[*]}"
	if [[ "$current_profile_list" == "${DESIRED_PROFILES[*]}" ]]; then
		printf 'Kubernetes compatibility profiles already match %s\n' "$stable_version"
		return 0
	fi
	WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kcp-compatibility.XXXXXX")"
	trap 'rm -rf "$WORK_DIR"' EXIT
	for profile in "${DESIRED_PROFILES[@]}"; do prepare_profile_data "$profile"; done
	write_versions_file
	write_versions_test_file
	write_compatibility_workflow
	write_compatibility_document
	write_readme_compatibility_badge
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
