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
COVERAGE_HTML="${KCP_COVERAGE_HTML:-$ROOT/.codex/reports/coverage.html}"
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
declare -a COVERAGE_PROXY_PIDS=()
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

  for pid in "${COVERAGE_PROXY_PIDS[@]:-}"; do
    printf '%s\n' "$pid" >>"$pids_file"
  done

  for state_path in "$STATE_FILE" "$RO_STATE_FILE"; do
    proxy_pids_for_state "$state_path" >>"$pids_file"
  done

  awk 'NF && !seen[$0]++' "$pids_file" >"${pids_file}.unique"
  mv "${pids_file}.unique" "$pids_file"

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

# Invoked indirectly through run_cmd.
# shellcheck disable=SC2329
start_coverage_proxy() {
  local state_path="$1"
  local log_path="${state_path}.log"
  local pid
  local attempt

  "$BINARY" serve --state "$state_path" >/dev/null 2>&1 &
  pid=$!
  COVERAGE_PROXY_PIDS+=("$pid")

  for ((attempt = 0; attempt < 100; attempt++)); do
    if grep -q 'listen:' "$log_path" 2>/dev/null; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid"
      return 1
    fi
    sleep 0.1
  done

  printf 'coverage proxy did not become ready: %s\n' "$state_path" >&2
  return 1
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
  if ! mkdir -p "$(dirname "$COVERAGE_HTML")"; then
    add_result "FAIL" "integration coverage report" "could not create report directory for $COVERAGE_HTML"
    return 1
  fi
  if ! GOTOOLCHAIN=auto go tool cover -html="$COVERAGE_PROFILE" -o "$COVERAGE_HTML"; then
    add_result "FAIL" "integration coverage report" "could not write $COVERAGE_HTML"
    return 1
  fi
  total="$(printf '%s\n' "$report" | awk '$1 == "total:" { print $NF }')"
  if [[ -z "$total" ]]; then
    add_result "FAIL" "integration coverage report" "total coverage is missing"
    return 1
  fi
  COVERAGE_REPORT="$report"
  add_result "PASS" "integration coverage report" "$total of statements; HTML: $COVERAGE_HTML"
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

check_proxy_log() {
  local state_path="$1"
  local log_path="${state_path}.log"

  if [[ -s "$log_path" ]]; then
    add_result "PASS" "proxy serve logging" "$log_path"
  else
    add_result "FAIL" "proxy serve logging" "expected non-empty $log_path"
  fi
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
    kubectl_ctx "$ctx" -n "$NS" delete pod kcp-debug-pod kcp-subresource-pod --ignore-not-found >/dev/null 2>&1 || true
    kubectl_ctx "$ctx" -n "$NS" delete configmap \
    kcp-only-a kcp-only-b kcp-fanout kcp-target-b kcp-single kcp-delete-a kcp-readonly kcp-watch-a kcp-watch-b \
      --ignore-not-found >/dev/null 2>&1 || true
    kubectl_ctx "$ctx" -n "$NS" delete secret sh.helm.release.v1.kcp.v1 --ignore-not-found >/dev/null 2>&1 || true
  done
}

# shellcheck source=e2e/checks/context.sh
source "$ROOT/e2e/checks/context.sh"
# shellcheck source=e2e/checks/aggregation.sh
source "$ROOT/e2e/checks/aggregation.sh"
# shellcheck source=e2e/checks/routing.sh
source "$ROOT/e2e/checks/routing.sh"
# shellcheck source=e2e/checks/subresources.sh
source "$ROOT/e2e/checks/subresources.sh"
# shellcheck source=e2e/checks/watch.sh
source "$ROOT/e2e/checks/watch.sh"
# shellcheck source=e2e/checks/werf.sh
source "$ROOT/e2e/checks/werf.sh"

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
require_cmd curl
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

if [[ "$COVERAGE_ENABLED" == "1" ]]; then
  if start_coverage_proxy "$STATE_FILE"; then
    add_result "PASS" "start coverage proxy" "ok"
  else
    add_result "FAIL" "start coverage proxy" "could not start $STATE_FILE"
  fi
fi
run_cmd "proxy discovery through exec credential" kubectl_ctx "$PROXY_CONTEXT" version

run_cmd "add read-only proxy context" "$BINARY" add-context "$RO_PROXY_CONTEXT" \
  --kubeconfig "$KUBECONFIG_FILE" \
  --state "$RO_STATE_FILE" \
  --contexts "$CTX_A,$CTX_B" \
  --primary-context "$CTX_A" \
  --listen "127.0.0.1:0" \
  --proxy-ttl "2m" \
  --request-timeout "$TIMEOUT" \
  --read-only \
  --logs-enabled \
  --exec-command "$BINARY"

if [[ "$COVERAGE_ENABLED" == "1" ]]; then
  if start_coverage_proxy "$RO_STATE_FILE"; then
    add_result "PASS" "start read-only coverage proxy" "ok"
  else
    add_result "FAIL" "start read-only coverage proxy" "could not start $RO_STATE_FILE"
  fi
fi

run_context_checks
run_aggregation_checks
check_proxy_log "$STATE_FILE"
check_proxy_log "$RO_STATE_FILE"
run_routing_checks
run_subresource_checks
run_watch_checks
run_werf_checks

cleanup_test_resources

if [[ "$HAD_FAILURE" -ne 0 ]]; then
  exit 1
fi
exit 0
