#!/usr/bin/env bash

set -u -o pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$ROOT" || exit 1

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kcp-it.XXXXXX")"
KUBECONFIG_FILE="$TMP_DIR/kubeconfig"
STATE_FILE="$TMP_DIR/kind-proxy.yaml"
RO_STATE_FILE="$TMP_DIR/kind-proxy-readonly.yaml"
PROXY_CONTEXT="kind-proxy"
RO_PROXY_CONTEXT="kind-proxy-readonly"
HASHED_PROXY_CONTEXT="kind/proxy-state"
SAFE_PROXY_CONTEXT="kind_proxy-state"
DUPLICATE_PROXY_CONTEXT="kind-proxy-duplicate"
CTX_A="kind-kubeconfig-proxy-a"
CTX_B="kind-kubeconfig-proxy-b"
NS="default"
WERF_NS="${KCP_WERF_NAMESPACE:-kcp-werf-$$}"
TIMEOUT="${KCP_TEST_TIMEOUT:-30s}"
CLUSTER_READY_TIMEOUT="${KCP_CLUSTER_READY_TIMEOUT:-120s}"
WERF_TIMEOUT="${KCP_WERF_TIMEOUT:-180}"
BINARY="$ROOT/bin/kubeconfig-proxy"
COVERAGE_DATA_DIR="$TMP_DIR/coverage-data"
COVERAGE_PROFILE="$TMP_DIR/integration-coverage.out"
KUBERNETES_VERSION="v1.36.1"
KUBECTL_VERSION="v1.36.1"
KIND_NODE_IMAGE="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
KUBECTL_BIN=""
KCP_CACHE_DIR="${KCP_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/kubeconfig-proxy}"
TOUCHED_TEST_RESOURCES=0
COVERAGE_ENABLED=0
COVERAGE_REPORT=""

declare -a RESULT_STATUS=()
declare -a RESULT_NAME=()
declare -a RESULT_DETAILS=()
declare -a CREATED_CLUSTERS=()
HAD_FAILURE=0

sanitize() {
  local value="${1//$'\n'/ }"
  value="${value//$'\r'/ }"
  value="${value//|/\\|}"
  printf '%s' "$value"
}

format_status() {
  case "$1" in
    PASS) printf '✅ PASS' ;;
    FAIL) printf '❌ FAIL' ;;
    SKIP) printf '⏭️ SKIP' ;;
    *) printf '%s' "$1" ;;
  esac
}

add_result() {
  local status="$1"
  local name="$2"
  local details="${3:-}"
  RESULT_STATUS+=("$status")
  RESULT_NAME+=("$name")
  RESULT_DETAILS+=("$(sanitize "$details")")
  case "$status" in
    FAIL) HAD_FAILURE=1 ;;
  esac
  printf '[%s] %s' "$(format_status "$status")" "$name"
  if [[ -n "$details" ]]; then
    printf ' - %s' "$details"
  fi
  printf '\n'
}

# Called from cleanup, which is invoked through trap.
# shellcheck disable=SC2329
print_results() {
  printf '\n| Status | Check | Details |\n'
  printf '| --- | --- | --- |\n'
  local i
  for i in "${!RESULT_STATUS[@]}"; do
    printf '| %s | %s | %s |\n' "$(format_status "${RESULT_STATUS[$i]}")" "${RESULT_NAME[$i]}" "${RESULT_DETAILS[$i]}"
  done
}

# Called from cleanup, which is invoked through trap.
# shellcheck disable=SC2329
print_coverage_report() {
  if [[ -z "$COVERAGE_REPORT" ]]; then
    return
  fi
  printf '\n## Integration coverage\n\n'
  printf '%s\n' '```text'
  printf '%s\n' "$COVERAGE_REPORT"
  printf '%s\n' '```'
}

# Called from stop_coverage_proxies during cleanup.
# shellcheck disable=SC2329
proxy_pids_for_state() {
  local state_path="$1"
  ps -eo pid=,command= | awk -v expected=" serve --state $state_path" '
    {
      pid = $1
      executable = $2
      $1 = ""
      sub(/^[[:space:]]+/, "", $0)
      if (executable ~ /(^|\/)kubeconfig-proxy$/ && index($0, expected) > 0) {
        print pid
      }
    }
  '
}

# Called from cleanup, which is invoked through trap.
# shellcheck disable=SC2329
stop_coverage_proxies() {
  local state_path
  local pid
  local attempt
  local stopped=0
  local pids_file="$TMP_DIR/coverage-proxy-pids"

  : >"$pids_file"

  for state_path in "$STATE_FILE" "$RO_STATE_FILE"; do
    proxy_pids_for_state "$state_path" >>"$pids_file"
  done

  while IFS= read -r pid; do
    if [[ -z "$pid" ]]; then
      continue
    fi
    if kill -TERM "$pid" 2>/dev/null; then
      stopped=$((stopped + 1))
    fi
  done <"$pids_file"

  while IFS= read -r pid; do
    if [[ -z "$pid" ]]; then
      continue
    fi
    for ((attempt = 0; attempt < 100; attempt++)); do
      if ! kill -0 "$pid" 2>/dev/null; then
        break
      fi
      sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
      add_result "FAIL" "stop coverage proxy processes" "process $pid did not exit"
      return 1
    fi
  done <"$pids_file"

  add_result "PASS" "stop coverage proxy processes" "$stopped process(es) stopped"
}

# Called from cleanup, which is invoked through trap.
# shellcheck disable=SC2329
finalize_integration_coverage() {
  local report
  local total

  if ! find "$COVERAGE_DATA_DIR" -type f -name 'covmeta.*' -print -quit | grep -q .; then
    add_result "FAIL" "integration coverage report" "coverage binary did not write metadata"
    return 1
  fi
  if ! GOTOOLCHAIN=auto go tool covdata textfmt -i="$COVERAGE_DATA_DIR" -o="$COVERAGE_PROFILE"; then
    add_result "FAIL" "integration coverage report" "could not convert coverage data"
    return 1
  fi
  if ! report="$(GOTOOLCHAIN=auto go tool cover -func="$COVERAGE_PROFILE" 2>&1)"; then
    add_result "FAIL" "integration coverage report" "$report"
    return 1
  fi
  total="$(printf '%s\n' "$report" | awk '$1 == "total:" { print $NF }')"
  if [[ -z "$total" ]]; then
    add_result "FAIL" "integration coverage report" "total coverage is missing"
    return 1
  fi
  COVERAGE_REPORT="$report"
  add_result "PASS" "integration coverage report" "$total of statements"
}

# Invoked by the EXIT trap.
# shellcheck disable=SC2329
cleanup() {
  local code=$?

  if [[ "$COVERAGE_ENABLED" == "1" ]]; then
    if ! stop_coverage_proxies; then
      code=1
    fi
  fi

  if [[ "${KCP_KEEP_KIND:-0}" == "1" ]]; then
    add_result "SKIP" "cleanup" "KCP_KEEP_KIND=1, leaving $TMP_DIR and kind clusters"
  else
    if [[ -x "$BINARY" && -f "$KUBECONFIG_FILE" ]]; then
      KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
      KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$RO_PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
      HOME="$TMP_DIR/home" KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$HASHED_PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
      HOME="$TMP_DIR/home" KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$SAFE_PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
      KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$DUPLICATE_PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
    fi

    if [[ "$TOUCHED_TEST_RESOURCES" == "1" && -n "${KUBECTL_BIN:-}" && -f "$KUBECONFIG_FILE" ]]; then
      KUBECONFIG="$KUBECONFIG_FILE" "$KUBECTL_BIN" --request-timeout="$TIMEOUT" --context "$CTX_A" delete namespace "$WERF_NS" --ignore-not-found >/dev/null 2>&1 || true
      KUBECONFIG="$KUBECONFIG_FILE" "$KUBECTL_BIN" --request-timeout="$TIMEOUT" --context "$CTX_B" delete namespace "$WERF_NS" --ignore-not-found >/dev/null 2>&1 || true
    fi

    local cluster
    if [[ "${#CREATED_CLUSTERS[@]}" -gt 0 ]]; then
      for cluster in "${CREATED_CLUSTERS[@]}"; do
        kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
      done
    fi
  fi

  if [[ "$COVERAGE_ENABLED" == "1" ]]; then
    if ! finalize_integration_coverage; then
      code=1
    fi
  fi

  if [[ "${KCP_KEEP_KIND:-0}" != "1" ]]; then
    rm -rf "$TMP_DIR"
  fi
  if [[ "$HAD_FAILURE" -ne 0 ]]; then
    code=1
  fi
  print_results
  print_coverage_report
  exit "$code"
}
trap cleanup EXIT

run_cmd() {
  local name="$1"
  shift
  local output
  if output="$("$@" 2>&1)"; then
    add_result "PASS" "$name" "ok"
    return 0
  fi
  add_result "FAIL" "$name" "$output"
  return 1
}

require_cmd() {
  local cmd="$1"
  if command -v "$cmd" >/dev/null 2>&1; then
    add_result "PASS" "required tool: $cmd" "$(command -v "$cmd")"
    return 0
  fi
  add_result "FAIL" "required tool: $cmd" "not found in PATH"
  return 1
}

kubectl_cmd() {
  KUBECONFIG="$KUBECONFIG_FILE" "$KUBECTL_BIN" --request-timeout="$TIMEOUT" "$@"
}

kubectl_ctx() {
  local ctx="$1"
  shift
  kubectl_cmd --context "$ctx" "$@"
}

expect_not_found() {
  local name="$1"
  shift
  local output
  if output="$("$@" 2>&1)"; then
    add_result "FAIL" "$name" "resource unexpectedly exists: $output"
    return 1
  fi
  if [[ "$output" == *"NotFound"* || "$output" == *"not found"* ]]; then
    add_result "PASS" "$name" "not found"
    return 0
  fi
  add_result "FAIL" "$name" "$output"
  return 1
}

expect_exists() {
  local name="$1"
  shift
  local output
  if output="$("$@" 2>&1)"; then
    add_result "PASS" "$name" "exists"
    return 0
  fi
  add_result "FAIL" "$name" "$output"
  return 1
}

expect_namespace_deleted_or_terminating() {
  local name="$1"
  local ctx="$2"
  local output
  if ! output="$(kubectl_ctx "$ctx" get namespace "$WERF_NS" -o jsonpath='{.status.phase}' 2>&1)"; then
    if [[ "$output" == *"NotFound"* || "$output" == *"not found"* ]]; then
      add_result "PASS" "$name" "namespace is gone"
      return 0
    fi
    add_result "FAIL" "$name" "$output"
    return 1
  fi
  if [[ "$output" == "Terminating" ]]; then
    add_result "PASS" "$name" "namespace is terminating"
    return 0
  fi
  add_result "FAIL" "$name" "namespace phase is $output"
  return 1
}

cluster_exists() {
  local cluster="$1"
  kind get clusters 2>/dev/null | grep -Fxq "$cluster"
}

detect_kubectl_platform() {
  local os
  local arch
  case "$(uname -s)" in
    Darwin) os="darwin" ;;
    Linux) os="linux" ;;
    *) return 1 ;;
  esac
  case "$(uname -m)" in
    arm64 | aarch64) arch="arm64" ;;
    x86_64 | amd64) arch="amd64" ;;
    *) return 1 ;;
  esac
  printf '%s/%s' "$os" "$arch"
}

sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
    return 0
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
    return 0
  fi
  return 1
}

kubectl_client_version() {
  local kubectl="$1"
  "$kubectl" version --client=true -o json 2>/dev/null |
    awk -F'"' '/"gitVersion"[[:space:]]*:/ {print $4; exit}'
}

download_pinned_kubectl() {
  local platform
  local os
  local arch
  local url
  local checksum_url
  local checksum
  local actual
  local cache_dir
  local cache_bin
  local tmp_bin

  if ! command -v curl >/dev/null 2>&1; then
    add_result "FAIL" "required tool: curl" "not found in PATH; needed to download kubectl $KUBECTL_VERSION"
    return 1
  fi
  add_result "PASS" "required tool: curl" "$(command -v curl)"

  if ! platform="$(detect_kubectl_platform)"; then
    add_result "FAIL" "kubectl $KUBECTL_VERSION" "unsupported platform $(uname -s)/$(uname -m)"
    return 1
  fi
  os="${platform%/*}"
  arch="${platform#*/}"
  url="https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$os/$arch/kubectl"
  checksum_url="$url.sha256"
  cache_dir="$KCP_CACHE_DIR/kubectl/$os/$arch/$KUBECTL_VERSION"
  cache_bin="$cache_dir/kubectl-${KUBECTL_VERSION}"

  if ! checksum="$(curl -fsSL "$checksum_url")"; then
    add_result "FAIL" "download kubectl $KUBECTL_VERSION checksum" "$checksum_url"
    return 1
  fi

  if [[ -x "$cache_bin" ]]; then
    if ! actual="$(sha256_file "$cache_bin")"; then
      add_result "FAIL" "verify cached kubectl $KUBECTL_VERSION checksum" "sha256sum or shasum is required"
      return 1
    fi
    if [[ "$actual" == "$checksum" ]] && [[ "$(kubectl_client_version "$cache_bin")" == "$KUBECTL_VERSION" ]]; then
      KUBECTL_BIN="$cache_bin"
      add_result "PASS" "kubectl $KUBECTL_VERSION" "using cached $cache_bin"
      return 0
    fi
    add_result "SKIP" "cached kubectl $KUBECTL_VERSION" "cache miss or checksum mismatch at $cache_bin"
  fi

  if ! mkdir -p "$cache_dir"; then
    add_result "FAIL" "kubectl cache" "could not create $cache_dir"
    return 1
  fi
  tmp_bin="$cache_dir/kubectl.$$"
  if ! curl -fsSL -o "$tmp_bin" "$url"; then
    add_result "FAIL" "download kubectl $KUBECTL_VERSION" "$url"
    rm -f "$tmp_bin"
    return 1
  fi
  if ! actual="$(sha256_file "$tmp_bin")"; then
    add_result "FAIL" "verify kubectl $KUBECTL_VERSION checksum" "sha256sum or shasum is required"
    rm -f "$tmp_bin"
    return 1
  fi
  if [[ "$actual" != "$checksum" ]]; then
    add_result "FAIL" "verify kubectl $KUBECTL_VERSION checksum" "got $actual, want $checksum"
    rm -f "$tmp_bin"
    return 1
  fi
  chmod +x "$tmp_bin"
  if ! mv "$tmp_bin" "$cache_bin"; then
    add_result "FAIL" "kubectl cache" "could not move $tmp_bin to $cache_bin"
    rm -f "$tmp_bin"
    return 1
  fi
  KUBECTL_BIN="$cache_bin"
  add_result "PASS" "kubectl $KUBECTL_VERSION" "downloaded and cached $cache_bin"
}

setup_kubectl() {
  local local_kubectl
  local local_version

  if command -v kubectl >/dev/null 2>&1; then
    local_kubectl="$(command -v kubectl)"
    local_version="$(kubectl_client_version "$local_kubectl")"
    if [[ "$local_version" == "$KUBECTL_VERSION" ]]; then
      KUBECTL_BIN="$local_kubectl"
      add_result "PASS" "kubectl $KUBECTL_VERSION" "$KUBECTL_BIN"
      return 0
    fi
    add_result "SKIP" "local kubectl $KUBECTL_VERSION" "found ${local_version:-unknown} at $local_kubectl; downloading pinned client"
  else
    add_result "SKIP" "local kubectl $KUBECTL_VERSION" "not found in PATH; downloading pinned client"
  fi

  download_pinned_kubectl
}

cluster_server_version() {
  local ctx="$1"
  kubectl_ctx "$ctx" version -o json 2>/dev/null |
    awk -F'"' 'seen && /"gitVersion"[[:space:]]*:/ {print $4; exit} /"serverVersion"[[:space:]]*:/ {seen=1}'
}

check_cluster_version() {
  local cluster="$1"
  local ctx="$2"
  local version
  if ! version="$(cluster_server_version "$ctx")" || [[ -z "$version" ]]; then
    add_result "FAIL" "kind cluster $cluster Kubernetes version" "could not read server version for $ctx"
    return 1
  fi
  if [[ "$version" == "$KUBERNETES_VERSION" ]]; then
    add_result "PASS" "kind cluster $cluster Kubernetes version" "$version"
    return 0
  fi
  add_result "FAIL" "kind cluster $cluster Kubernetes version" "got $version, want $KUBERNETES_VERSION; rerun with KCP_RECREATE_KIND=1"
  return 1
}

check_cluster_ready() {
  local cluster="$1"
  local ctx="$2"
  local output
  local nodes
  if output="$(kubectl_ctx "$ctx" wait --for=condition=Ready nodes --all --timeout="$CLUSTER_READY_TIMEOUT" 2>&1)"; then
    add_result "PASS" "kind cluster $cluster node readiness" "$output"
    return 0
  fi
  nodes="$(kubectl_ctx "$ctx" get nodes -o wide 2>&1 || true)"
  add_result "FAIL" "kind cluster $cluster node readiness" "$output; nodes: $nodes"
  return 1
}

ensure_cluster() {
  local cluster="$1"
  local ctx="kind-$cluster"
  if cluster_exists "$cluster"; then
    if [[ "${KCP_RECREATE_KIND:-0}" == "1" ]]; then
      run_cmd "delete existing kind cluster $cluster" kind delete cluster --name "$cluster" || return 1
    else
      run_cmd "export existing kind cluster $cluster" kind export kubeconfig --name "$cluster" --kubeconfig "$KUBECONFIG_FILE" || return 1
      check_cluster_version "$cluster" "$ctx" || return 1
      check_cluster_ready "$cluster" "$ctx" || return 1
      add_result "PASS" "kind cluster $cluster" "reused existing cluster"
      return 0
    fi
  fi

  if run_cmd "create kind cluster $cluster" kind create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --kubeconfig "$KUBECONFIG_FILE"; then
    CREATED_CLUSTERS+=("$cluster")
    check_cluster_version "$cluster" "$ctx" || return 1
    check_cluster_ready "$cluster" "$ctx" || return 1
    return 0
  fi
  return 1
}

cleanup_test_resources() {
  local ctx
  for ctx in "$CTX_A" "$CTX_B"; do
    kubectl_ctx "$ctx" -n "$NS" delete pod kcp-subresource-pod --ignore-not-found >/dev/null 2>&1 || true
    kubectl_ctx "$ctx" -n "$NS" delete serviceaccount kcp-subresource-sa --ignore-not-found >/dev/null 2>&1 || true
    kubectl_ctx "$ctx" -n "$NS" delete configmap \
    kcp-only-a kcp-only-b kcp-fanout kcp-target-b kcp-single kcp-delete-a kcp-readonly \
      --ignore-not-found >/dev/null 2>&1 || true
    kubectl_ctx "$ctx" -n "$NS" delete secret sh.helm.release.v1.kcp.v1 --ignore-not-found >/dev/null 2>&1 || true
  done
}

# Invoked indirectly via run_cmd.
# shellcheck disable=SC2329
apply_manifest() {
  local ctx="$1"
  local file="$2"
  kubectl_ctx "$ctx" apply -f "$file"
}

write_configmap_manifest() {
  local file="$1"
  local name="$2"
  local annotations="$3"
  cat >"$file" <<EOF_MANIFEST
apiVersion: v1
kind: ConfigMap
metadata:
  name: $name
  namespace: $NS
$annotations
data:
  value: "$name"
EOF_MANIFEST
}

if [[ "${KCP_SKIP_MAKE_CHECK:-0}" == "1" ]]; then
  add_result "SKIP" "make check" "KCP_SKIP_MAKE_CHECK=1"
else
  run_cmd "make check" make check
fi

if ! mkdir -p "$COVERAGE_DATA_DIR"; then
  add_result "FAIL" "coverage data directory" "could not create $COVERAGE_DATA_DIR"
elif run_cmd "build coverage binary" make build-cover; then
  export GOCOVERDIR="$COVERAGE_DATA_DIR"
  COVERAGE_ENABLED=1
  run_cmd "coverage binary instrumentation" "$BINARY" version
fi

require_cmd kind
setup_kubectl
if [[ "${KCP_SKIP_WERF:-0}" == "1" ]]; then
  add_result "SKIP" "required tool: werf" "KCP_SKIP_WERF=1"
else
  require_cmd werf
fi

if [[ ! -x "$BINARY" ]]; then
  add_result "FAIL" "coverage binary" "$BINARY is missing or not executable"
else
  add_result "PASS" "coverage binary" "$BINARY"
fi

if [[ "$HAD_FAILURE" -ne 0 ]]; then
  add_result "SKIP" "integration checks" "previous setup check failed"
  exit 1
fi

ensure_cluster kubeconfig-proxy-a
ensure_cluster kubeconfig-proxy-b

if [[ "$HAD_FAILURE" -ne 0 ]]; then
  add_result "SKIP" "integration checks" "previous setup check failed"
  exit 1
fi

TOUCHED_TEST_RESOURCES=1
cleanup_test_resources

run_cmd "add proxy context" "$BINARY" add-context "$PROXY_CONTEXT" \
  --kubeconfig "$KUBECONFIG_FILE" \
  --state "$STATE_FILE" \
  --contexts "$CTX_A,$CTX_B" \
  --primary-context "$CTX_A" \
  --listen "127.0.0.1:0" \
  --proxy-ttl "2m" \
  --request-timeout "$TIMEOUT" \
  --helm-release-proxy \
  --logs-enabled \
  --exec-command "$BINARY"

run_cmd "add read-only proxy context" "$BINARY" add-context "$RO_PROXY_CONTEXT" \
  --kubeconfig "$KUBECONFIG_FILE" \
  --state "$RO_STATE_FILE" \
  --contexts "$CTX_A,$CTX_B" \
  --primary-context "$CTX_A" \
  --listen "127.0.0.1:0" \
  --proxy-ttl "2m" \
  --request-timeout "$TIMEOUT" \
  --read-only \
  --exec-command "$BINARY"

duplicate_output="$("$BINARY" add-context "$DUPLICATE_PROXY_CONTEXT" \
  --kubeconfig "$KUBECONFIG_FILE" \
  --state "$TMP_DIR/duplicate.yaml" \
  --contexts "$CTX_A,$CTX_A" \
  --listen "127.0.0.1:0" \
  --exec-command "$BINARY" 2>&1)"
duplicate_status=$?
if [[ "$duplicate_status" -ne 0 && "$duplicate_output" == *"selected more than once"* && ! -f "$TMP_DIR/duplicate.yaml" ]]; then
  add_result "PASS" "duplicate source contexts are rejected" "state was not written"
else
  add_result "FAIL" "duplicate source contexts are rejected" "status=$duplicate_status output=$duplicate_output"
fi

run_cmd "add proxy context with hashed default state path" env HOME="$TMP_DIR/home" "$BINARY" add-context "$HASHED_PROXY_CONTEXT" \
  --kubeconfig "$KUBECONFIG_FILE" \
  --contexts "$CTX_A,$CTX_B" \
  --listen "127.0.0.1:0" \
  --exec-command "$BINARY"
run_cmd "add proxy context with safe default state path" env HOME="$TMP_DIR/home" "$BINARY" add-context "$SAFE_PROXY_CONTEXT" \
  --kubeconfig "$KUBECONFIG_FILE" \
  --contexts "$CTX_A,$CTX_B" \
  --listen "127.0.0.1:0" \
  --exec-command "$BINARY"
state_file_count="$(find "$TMP_DIR/home/.kube/kubeconfig-proxy" -type f -name '*.yaml' | wc -l | tr -d ' ')"
if [[ "$state_file_count" == "2" ]]; then
  add_result "PASS" "default state paths avoid sanitized-name collisions" "created two distinct state files"
else
  add_result "FAIL" "default state paths avoid sanitized-name collisions" "found $state_file_count state files"
fi

run_cmd "proxy discovery and exec credential auto-start" kubectl_ctx "$PROXY_CONTEXT" version

run_cmd "seed aggregate resources in source clusters" bash -c "
  set -euo pipefail
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create configmap kcp-only-a --from-literal=value=a
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' label configmap kcp-only-a kcp-pagination=yes
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create configmap kcp-only-b --from-literal=value=b
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' label configmap kcp-only-b kcp-pagination=yes
"

aggregate_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
if [[ "$aggregate_output" == *"kcp-only-a=$CTX_A"* && "$aggregate_output" == *"kcp-only-b=$CTX_B"* ]]; then
  add_result "PASS" "aggregated list adds context labels" "saw kcp-only-a=$CTX_A and kcp-only-b=$CTX_B"
else
  add_result "FAIL" "aggregated list adds context labels" "$aggregate_output"
fi

paginated_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps -l kcp-pagination=yes --chunk-size=1 -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
paginated_count="$(printf '%s\n' "$paginated_output" | sed '/^$/d' | wc -l | tr -d ' ')"
if [[ "$paginated_count" == "2" && "$paginated_output" == *"kcp-only-a=$CTX_A"* && "$paginated_output" == *"kcp-only-b=$CTX_B"* ]]; then
  add_result "PASS" "aggregated list pagination crosses target boundary" "chunk-size=1 returned both contexts exactly once"
else
  add_result "FAIL" "aggregated list pagination crosses target boundary" "$paginated_output"
fi

readonly_list_output="$(kubectl_ctx "$RO_PROXY_CONTEXT" -n "$NS" get configmaps -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
if [[ "$readonly_list_output" == *"kcp-only-a=$CTX_A"* && "$readonly_list_output" == *"kcp-only-b=$CTX_B"* ]]; then
  add_result "PASS" "read-only proxy allows list reads" "saw kcp-only-a=$CTX_A and kcp-only-b=$CTX_B"
else
  add_result "FAIL" "read-only proxy allows list reads" "$readonly_list_output"
fi

run_cmd "fan-out create mutation" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" create configmap kcp-fanout --from-literal=value=shared
expect_exists "fan-out object exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap kcp-fanout
expect_exists "fan-out object exists in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap kcp-fanout

target_b_manifest="$TMP_DIR/context-name.yaml"
write_configmap_manifest "$target_b_manifest" "kcp-target-b" "  annotations:
    kubeconfig-proxy.io/context-name: $CTX_B"
run_cmd "context-name routes create to kubeconfig-proxy-b" apply_manifest "$PROXY_CONTEXT" "$target_b_manifest"
expect_not_found "context-name object absent from kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap kcp-target-b
expect_exists "context-name object exists in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap kcp-target-b

named_get_value="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmap kcp-target-b -o jsonpath='{.metadata.name}' 2>&1)"
if [[ "$named_get_value" == "kcp-target-b" ]]; then
  add_result "PASS" "named GET routes to cluster containing object" "found object through proxy"
else
  add_result "FAIL" "named GET routes to cluster containing object" "$named_get_value"
fi

single_manifest="$TMP_DIR/single-context.yaml"
write_configmap_manifest "$single_manifest" "kcp-single" "  annotations:
    kubeconfig-proxy.io/single-context: \"true\""
run_cmd "single-context routes create to first context" apply_manifest "$PROXY_CONTEXT" "$single_manifest"
expect_exists "single-context object exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap kcp-single
expect_not_found "single-context object absent from kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap kcp-single

run_cmd "seed service account for logs and exec in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" create serviceaccount kcp-subresource-sa
run_cmd "seed pod for logs and exec in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" run kcp-subresource-pod \
  --image=busybox:1.37 \
  --restart=Never \
  --overrides='{"spec":{"serviceAccountName":"kcp-subresource-sa","automountServiceAccountToken":false}}' \
  --command -- sh -c 'echo kcp-log-from-kubeconfig-proxy-b; sleep 300'
run_cmd "wait for logs and exec pod readiness" kubectl_ctx "$CTX_B" -n "$NS" wait --for=condition=Ready pod/kcp-subresource-pod --timeout=90s

logs_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" logs kcp-subresource-pod 2>&1)"
if [[ "$logs_output" == *"kcp-log-from-kubeconfig-proxy-b"* ]]; then
  add_result "PASS" "kubectl logs routes to cluster containing pod" "read kubeconfig-proxy-b pod logs"
else
  add_result "FAIL" "kubectl logs routes to cluster containing pod" "$logs_output"
fi

exec_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" exec kcp-subresource-pod -- sh -c 'echo kcp-exec-from-kubeconfig-proxy-b' 2>&1)"
if [[ "$exec_output" == *"kcp-exec-from-kubeconfig-proxy-b"* ]]; then
  add_result "PASS" "kubectl exec routes to cluster containing pod" "executed command in kubeconfig-proxy-b pod"
else
  add_result "FAIL" "kubectl exec routes to cluster containing pod" "$exec_output"
fi

run_cmd "PATCH uses existing object routing" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" patch configmap kcp-target-b --type merge -p '{"data":{"patched":"yes"}}'
patch_value="$(kubectl_ctx "$CTX_B" -n "$NS" get configmap kcp-target-b -o jsonpath='{.data.patched}' 2>&1)"
if [[ "$patch_value" == "yes" ]]; then
  add_result "PASS" "PATCH changed only annotated target" "kubeconfig-proxy-b patched"
else
  add_result "FAIL" "PATCH changed only annotated target" "$patch_value"
fi
expect_not_found "PATCH did not create object in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap kcp-target-b

run_cmd "seed delete-only resource in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" create configmap kcp-delete-a --from-literal=value=a
run_cmd "DELETE routes only where named object exists" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" delete configmap kcp-delete-a
expect_not_found "DELETE removed object from kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap kcp-delete-a

readonly_output="$(kubectl_ctx "$RO_PROXY_CONTEXT" -n "$NS" create configmap kcp-readonly --from-literal=value=blocked 2>&1)"
if [[ "$readonly_output" == *"Forbidden"* || "$readonly_output" == *"read-only proxy rejects"* ]]; then
  add_result "PASS" "read-only proxy rejects mutation" "$readonly_output"
else
  add_result "FAIL" "read-only proxy rejects mutation" "$readonly_output"
fi
expect_not_found "read-only did not create object in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap kcp-readonly
expect_not_found "read-only did not create object in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap kcp-readonly

run_cmd "seed helm release storage in both clusters" bash -c "
  set -euo pipefail
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create secret generic sh.helm.release.v1.kcp.v1 --from-literal=release=a --dry-run=client -o yaml | KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' apply -f -
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' label secret sh.helm.release.v1.kcp.v1 owner=helm name=kcp --overwrite
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create secret generic sh.helm.release.v1.kcp.v1 --from-literal=release=b --dry-run=client -o yaml | KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' apply -f -
  KUBECONFIG='$KUBECONFIG_FILE' '$KUBECTL_BIN' --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' label secret sh.helm.release.v1.kcp.v1 owner=helm name=kcp --overwrite
"
helm_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get secrets -l owner=helm,name=kcp -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>&1)"
helm_count="$(printf '%s\n' "$helm_output" | sed '/^$/d' | wc -l | tr -d ' ')"
if [[ "$helm_count" == "1" && "$helm_output" == *"sh.helm.release.v1.kcp.v1"* ]]; then
  add_result "PASS" "helm-release-proxy reads release storage from primary only" "one release storage item returned"
else
  add_result "FAIL" "helm-release-proxy reads release storage from primary only" "$helm_output"
fi

if [[ "${KCP_SKIP_WERF:-0}" == "1" ]]; then
  add_result "SKIP" "werf example converge and dismiss" "KCP_SKIP_WERF=1"
else
  run_cmd "werf example converge" bash -c "
    set -euo pipefail
    cd '$ROOT/examples/werf'
    KUBECONFIG='$KUBECONFIG_FILE' werf converge --env kind --dev --namespace '$WERF_NS' --kube-context '$PROXY_CONTEXT' --timeout '$WERF_TIMEOUT'
  "

  expect_exists "werf nginx deployment exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$WERF_NS" get deployment kubeconfig-proxy-werf-nginx
  expect_exists "werf nginx deployment exists in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$WERF_NS" get deployment kubeconfig-proxy-werf-nginx
  expect_exists "werf single-context job exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$WERF_NS" get job kubeconfig-proxy-werf-smoke
  expect_not_found "werf single-context job absent from kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$WERF_NS" get job kubeconfig-proxy-werf-smoke

  run_cmd "werf example dismiss" bash -c "
    set -euo pipefail
    cd '$ROOT/examples/werf'
    KUBECONFIG='$KUBECONFIG_FILE' werf dismiss --env kind --with-namespace --namespace '$WERF_NS' --kube-context '$PROXY_CONTEXT'
  "
  expect_namespace_deleted_or_terminating "werf namespace removed from kubeconfig-proxy-a" "$CTX_A"
  expect_namespace_deleted_or_terminating "werf namespace removed from kubeconfig-proxy-b" "$CTX_B"
fi

cleanup_test_resources

if [[ "$HAD_FAILURE" -ne 0 ]]; then
  exit 1
fi
exit 0
