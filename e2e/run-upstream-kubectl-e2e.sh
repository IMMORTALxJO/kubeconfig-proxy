#!/usr/bin/env bash

# Runs the upstream [sig-cli] Kubectl client e2e suite through a single-source
# kubeconfig-proxy context. A single source is deliberate: the upstream suite
# expects one coherent Kubernetes API and cannot validate multi-cluster fan-out.

set -u -o pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$ROOT" || exit 1

KUBERNETES_VERSION="v1.36.1"
KUBERNETES_COMMIT="756939600b9a7180fc2df6550a4585b638875e67"
KIND_NODE_IMAGE="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
CLUSTER="kubeconfig-proxy-kubectl-e2e"
SOURCE_CONTEXT="kind-$CLUSTER"
PROXY_CONTEXT="kind-proxy-kubectl-e2e"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kcp-kubectl-e2e.XXXXXX")"
KUBECONFIG_FILE="$TMP_DIR/kubeconfig"
STATE_FILE="$TMP_DIR/proxy-state.yaml"
REPORT_DIR="$TMP_DIR/reports"
BINARY="$ROOT/bin/kubeconfig-proxy"
COVERAGE_DATA_DIR="${KCP_COVERAGE_DATA_DIR:-$TMP_DIR/coverage-data}"
COVERAGE_PROFILE="$TMP_DIR/upstream-kubectl-e2e-coverage.out"
COVERAGE_HTML="${KCP_COVERAGE_HTML:-$ROOT/.codex/reports/coverage.html}"
CLUSTER_READY_TIMEOUT="${KCP_CLUSTER_READY_TIMEOUT:-120s}"
E2E_FOCUS="${KCP_KUBECTL_E2E_FOCUS:-\[sig-cli\] Kubectl client}"
E2E_SKIP="${KCP_KUBECTL_E2E_SKIP-should handle in-cluster config}"
E2E_TIMEOUT="${KCP_KUBECTL_E2E_TIMEOUT:-6h}"
KCP_CACHE_DIR="${KCP_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/kubeconfig-proxy}"
KUBERNETES_SOURCE="${KCP_KUBERNETES_SOURCE:-$KCP_CACHE_DIR/kubernetes/$KUBERNETES_VERSION}"
BUILDER_IMAGE="${KCP_KUBECTL_E2E_BUILDER_IMAGE:-golang:1.26.0-bookworm}"
DOCKER_PULL_TIMEOUT_SECONDS="${KCP_KUBECTL_E2E_DOCKER_PULL_TIMEOUT_SECONDS:-300}"
UPSTREAM_KUBECTL_BIN=""
E2E_BIN=""
KUBECTL_WRAPPER=""
UPSTREAM_BASH=""
BUILD_MODE=""
BUILD_PLATFORM=""
CREATED_CLUSTER=0
HAD_FAILURE=0
COVERAGE_ENABLED=0
COVERAGE_REPORT=""
COVERAGE_PROXY_PID=""

declare -a RESULT_STATUS=()
declare -a RESULT_NAME=()
declare -a RESULT_DETAILS=()

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
  if [[ "$status" == "FAIL" ]]; then
    HAD_FAILURE=1
  fi
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
stop_proxy() {
  local pid
  local attempt
  local pids_file="$TMP_DIR/proxy-pids"
  local stopped=0

  : >"$pids_file"
  if [[ -n "$COVERAGE_PROXY_PID" ]]; then
    printf '%s\n' "$COVERAGE_PROXY_PID" >>"$pids_file"
  fi
  proxy_pids_for_state "$STATE_FILE" >>"$pids_file"
  awk 'NF && !seen[$0]++' "$pids_file" >"${pids_file}.unique"
  mv "${pids_file}.unique" "$pids_file"
  while IFS= read -r pid; do
    [[ -z "$pid" ]] && continue
    if kill -TERM "$pid" 2>/dev/null; then
      stopped=$((stopped + 1))
    fi
  done <"$pids_file"

  while IFS= read -r pid; do
    [[ -z "$pid" ]] && continue
    for ((attempt = 0; attempt < 100; attempt++)); do
      if ! kill -0 "$pid" 2>/dev/null; then
        break
      fi
      sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
      add_result "FAIL" "stop proxy process" "process $pid did not exit"
      return 1
    fi
  done <"$pids_file"

  if [[ "$stopped" -gt 0 ]]; then
    add_result "PASS" "stop proxy process" "$stopped process(es) stopped"
  fi
}

# Invoked indirectly through run_logged.
# shellcheck disable=SC2329
start_coverage_proxy() {
  local attempt

  "$BINARY" serve --state "$STATE_FILE" >/dev/null 2>&1 &
  COVERAGE_PROXY_PID=$!

  for ((attempt = 0; attempt < 100; attempt++)); do
    if grep -q 'listen:' "${STATE_FILE}.log" 2>/dev/null; then
      return 0
    fi
    if ! kill -0 "$COVERAGE_PROXY_PID" 2>/dev/null; then
      wait "$COVERAGE_PROXY_PID"
      return 1
    fi
    sleep 0.1
  done

  printf 'coverage proxy did not become ready: %s\n' "$STATE_FILE" >&2
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

  stop_proxy || code=1

  if [[ "${KCP_KEEP_KIND:-0}" == "1" ]]; then
    add_result "SKIP" "cleanup" "KCP_KEEP_KIND=1, leaving $TMP_DIR and kind cluster $CLUSTER"
  else
    if [[ -x "$BINARY" && -f "$KUBECONFIG_FILE" ]]; then
      KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
    fi
    if [[ "$CREATED_CLUSTER" == "1" ]]; then
      kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
      add_result "PASS" "cleanup" "deleted kind cluster $CLUSTER"
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

run_logged() {
  local name="$1"
  shift
  local log_file="$TMP_DIR/${#RESULT_STATUS[@]}.log"
  local output
  if "$@" >"$log_file" 2>&1; then
    add_result "PASS" "$name" "ok"
    return 0
  fi
  output="$(tail -n 40 "$log_file" 2>/dev/null || true)"
  add_result "FAIL" "$name" "$output"
  return 1
}

run_streamed() {
  local name="$1"
  shift
  local log_file="$TMP_DIR/${#RESULT_STATUS[@]}.log"
  local output

  printf '\n==> %s\n' "$name"
  if "$@" 2>&1 | tee "$log_file"; then
    add_result "PASS" "$name" "ok"
    return 0
  fi
  output="$(tail -n 40 "$log_file" 2>/dev/null || true)"
  add_result "FAIL" "$name" "$output"
  return 1
}

check_proxy_log() {
  local log_path="${STATE_FILE}.log"

  if [[ -s "$log_path" ]]; then
    add_result "PASS" "proxy serve logging" "$log_path"
  else
    add_result "FAIL" "proxy serve logging" "expected non-empty $log_path"
  fi
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

detect_build_platform() {
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
  BUILD_PLATFORM="$os/$arch"
}

select_build_environment() {
  local candidate
  local major
  local minor
  local -a candidates=()

  if [[ -n "${KCP_KUBECTL_E2E_BASH:-}" ]]; then
    candidates+=("$KCP_KUBECTL_E2E_BASH")
  fi
  candidates+=(/opt/homebrew/bin/bash /usr/local/bin/bash)
  candidates+=("$(command -v bash)")

  for candidate in "${candidates[@]}"; do
    [[ -x "$candidate" ]] || continue
    # shellcheck disable=SC2016 # The child Bash must expand BASH_VERSINFO.
    read -r major minor < <("$candidate" -c 'printf "%s %s\n" "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}"')
    if ((major > 4 || (major == 4 && minor >= 2))); then
      UPSTREAM_BASH="$candidate"
      BUILD_MODE="host"
      add_result "PASS" "Bash for Kubernetes build" "$candidate ($major.$minor)"
      return 0
    fi
  done

  if ! detect_build_platform; then
    add_result "FAIL" "Kubernetes build environment" "unsupported host platform $(uname -s)/$(uname -m)"
    return 1
  fi
  if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    add_result "FAIL" "Kubernetes build environment" "Kubernetes $KUBERNETES_VERSION requires Bash >=4.2; install Homebrew bash, set KCP_KUBECTL_E2E_BASH, or start Docker"
    return 1
  fi
  BUILD_MODE="docker"
  add_result "SKIP" "Bash for Kubernetes build" "Bash >=4.2 not found; building $BUILD_PLATFORM binaries in Docker"
  add_result "PASS" "Kubernetes build environment" "Docker cross-build for $BUILD_PLATFORM"
}

ensure_builder_image() {
  local pull_pid
  local elapsed
  local pull_log="$TMP_DIR/docker-pull.log"

  if [[ "$BUILD_MODE" != "docker" ]]; then
    return 0
  fi
  if docker image inspect "$BUILDER_IMAGE" >/dev/null 2>&1; then
    add_result "PASS" "Kubernetes builder image" "using cached $BUILDER_IMAGE"
    return 0
  fi
  if [[ ! "$DOCKER_PULL_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]]; then
    add_result "FAIL" "Kubernetes builder image" "KCP_KUBECTL_E2E_DOCKER_PULL_TIMEOUT_SECONDS must be a positive integer"
    return 1
  fi

  docker pull "$BUILDER_IMAGE" >"$pull_log" 2>&1 &
  pull_pid=$!
  for ((elapsed = 0; elapsed < DOCKER_PULL_TIMEOUT_SECONDS; elapsed++)); do
    if ! kill -0 "$pull_pid" 2>/dev/null; then
      if wait "$pull_pid"; then
        add_result "PASS" "Kubernetes builder image" "pulled $BUILDER_IMAGE"
        return 0
      fi
      add_result "FAIL" "Kubernetes builder image" "$(tail -n 40 "$pull_log" 2>/dev/null || true)"
      return 1
    fi
    sleep 1
  done

  kill -TERM "$pull_pid" 2>/dev/null || true
  sleep 1
  kill -KILL "$pull_pid" 2>/dev/null || true
  wait "$pull_pid" 2>/dev/null || true
  add_result "FAIL" "Kubernetes builder image" "timed out pulling $BUILDER_IMAGE after ${DOCKER_PULL_TIMEOUT_SECONDS}s"
  return 1
}

cluster_exists() {
  kind get clusters 2>/dev/null | grep -Fxq "$CLUSTER"
}

cluster_server_version() {
  KUBECONFIG="$KUBECONFIG_FILE" kubectl --context "$SOURCE_CONTEXT" version -o json 2>/dev/null |
    awk -F'"' 'seen && /"gitVersion"[[:space:]]*:/ {print $4; exit} /"serverVersion"[[:space:]]*:/ {seen=1}'
}

check_cluster() {
  local version
  local output

  if ! version="$(cluster_server_version)" || [[ "$version" != "$KUBERNETES_VERSION" ]]; then
    add_result "FAIL" "kind cluster Kubernetes version" "got ${version:-unknown}, want $KUBERNETES_VERSION; set KCP_KUBECTL_E2E_RECREATE_KIND=1"
    return 1
  fi
  add_result "PASS" "kind cluster Kubernetes version" "$version"

  if output="$(KUBECONFIG="$KUBECONFIG_FILE" kubectl --context "$SOURCE_CONTEXT" wait --for=condition=Ready nodes --all --timeout="$CLUSTER_READY_TIMEOUT" 2>&1)"; then
    add_result "PASS" "kind cluster node readiness" "$output"
    return 0
  fi
  add_result "FAIL" "kind cluster node readiness" "$output"
  return 1
}

ensure_cluster() {
  if cluster_exists; then
    if [[ "${KCP_KUBECTL_E2E_RECREATE_KIND:-0}" == "1" ]]; then
      run_logged "delete existing kind cluster" kind delete cluster --name "$CLUSTER" || return 1
    else
      run_logged "export existing kind cluster kubeconfig" kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG_FILE" || return 1
      check_cluster || return 1
      add_result "PASS" "kind cluster" "reused existing cluster $CLUSTER"
      return 0
    fi
  fi

  if run_logged "create kind cluster" kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" --kubeconfig "$KUBECONFIG_FILE"; then
    CREATED_CLUSTER=1
    check_cluster
    return $?
  fi
  return 1
}

ensure_kubernetes_source() {
  local source_commit

  if [[ -d "$KUBERNETES_SOURCE/.git" ]]; then
    if source_commit="$(git -C "$KUBERNETES_SOURCE" rev-parse HEAD 2>/dev/null)" && [[ "$source_commit" == "$KUBERNETES_COMMIT" ]]; then
      add_result "PASS" "Kubernetes source $KUBERNETES_VERSION" "using cached $KUBERNETES_SOURCE"
      return 0
    fi
    add_result "FAIL" "Kubernetes source $KUBERNETES_VERSION" "$KUBERNETES_SOURCE is not commit $KUBERNETES_COMMIT"
    return 1
  fi

  if [[ -e "$KUBERNETES_SOURCE" ]]; then
    add_result "FAIL" "Kubernetes source cache" "$KUBERNETES_SOURCE exists but is not a git checkout"
    return 1
  fi

  if ! mkdir -p "$(dirname "$KUBERNETES_SOURCE")"; then
    add_result "FAIL" "Kubernetes source cache" "could not create $(dirname "$KUBERNETES_SOURCE")"
    return 1
  fi
  if ! run_logged "clone Kubernetes source $KUBERNETES_VERSION" git clone --depth 1 --branch "$KUBERNETES_VERSION" https://github.com/kubernetes/kubernetes.git "$KUBERNETES_SOURCE"; then
    return 1
  fi
  if source_commit="$(git -C "$KUBERNETES_SOURCE" rev-parse HEAD 2>/dev/null)" && [[ "$source_commit" == "$KUBERNETES_COMMIT" ]]; then
    add_result "PASS" "verify Kubernetes source $KUBERNETES_VERSION" "$source_commit"
    return 0
  fi
  add_result "FAIL" "verify Kubernetes source $KUBERNETES_VERSION" "got ${source_commit:-unknown}, want $KUBERNETES_COMMIT"
  return 1
}

# Invoked through run_logged.
# shellcheck disable=SC2329
build_upstream_binaries() {
  if [[ "$BUILD_MODE" == "host" ]]; then
    (
      cd "$KUBERNETES_SOURCE" || exit 1
      PATH="$(dirname "$UPSTREAM_BASH"):$PATH" GOTOOLCHAIN=auto "$UPSTREAM_BASH" -c 'exec make WHAT="cmd/kubectl test/e2e/e2e.test"'
    )
    return
  fi

  docker run --rm \
    --volume "$KUBERNETES_SOURCE:/workspace/kubernetes" \
    --workdir /workspace/kubernetes \
    --env "KUBE_BUILD_PLATFORMS=$BUILD_PLATFORM" \
    --env GOTOOLCHAIN=local \
    "$BUILDER_IMAGE" \
    bash -lc 'make WHAT="cmd/kubectl test/e2e/e2e.test"'
}

find_upstream_binary() {
  local name="$1"
  find "$KUBERNETES_SOURCE/_output" -type f -name "$name" -perm -u+x -print -quit 2>/dev/null
}

write_kubectl_wrapper() {
  local source_server

  source_server="$(kubectl --kubeconfig "$KUBECONFIG_FILE" --context "$SOURCE_CONTEXT" config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
  if [[ -z "$source_server" ]]; then
    return 1
  fi

  KUBECTL_WRAPPER="$TMP_DIR/kubectl.sh"
  cat >"$KUBECTL_WRAPPER" <<'EOF_KUBECTL_WRAPPER'
#!/usr/bin/env bash

set -u

if [[ "${1:-}" == "path" && "$#" -eq 1 ]]; then
  printf '%s\n' "$KCP_UPSTREAM_KUBECTL_BIN"
  exit 0
fi

declare -a args=()
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --kubeconfig=* | --context=*)
      shift
      ;;
    --kubeconfig | --context)
      shift 2
      ;;
    --server=*)
      server="${1#--server=}"
      if [[ "$server" != "$KCP_SOURCE_SERVER" ]]; then
        args+=("$1")
      fi
      shift
      ;;
    --server)
      if [[ "$#" -gt 1 && "$2" == "$KCP_SOURCE_SERVER" ]]; then
        shift 2
      else
        args+=("$1")
        shift
        if [[ "$#" -gt 0 ]]; then
          args+=("$1")
          shift
        fi
      fi
      ;;
    *)
      args+=("$1")
      shift
      ;;
  esac
done

exec "$KCP_UPSTREAM_KUBECTL_BIN" \
  --kubeconfig="$KCP_PROXY_KUBECONFIG" \
  --context="$KCP_PROXY_CONTEXT" \
  "${args[@]}"
EOF_KUBECTL_WRAPPER
  chmod +x "$KUBECTL_WRAPPER"
  export KCP_UPSTREAM_KUBECTL_BIN="$UPSTREAM_KUBECTL_BIN"
  export KCP_PROXY_KUBECONFIG="$KUBECONFIG_FILE"
  export KCP_PROXY_CONTEXT="$PROXY_CONTEXT"
  export KCP_SOURCE_SERVER="$source_server"
}

# Invoked through run_logged.
# shellcheck disable=SC2329
run_upstream_kubectl_e2e() {
  local -a args=(
    --provider=skeleton
    --kubeconfig="$KUBECONFIG_FILE"
    --context="$SOURCE_CONTEXT"
    --kubectl-path="$KUBECTL_WRAPPER"
    --report-dir="$REPORT_DIR"
    --ginkgo.focus="$E2E_FOCUS"
    --ginkgo.timeout="$E2E_TIMEOUT"
    --ginkgo.v
  )
  if [[ -n "$E2E_SKIP" ]]; then
    args+=(--ginkgo.skip="$E2E_SKIP")
  fi
  "$E2E_BIN" "${args[@]}"
}

require_cmd git
require_cmd kind
require_cmd kubectl
require_cmd make
select_build_environment
ensure_builder_image

if ! mkdir -p "$COVERAGE_DATA_DIR"; then
  add_result "FAIL" "coverage data directory" "could not create $COVERAGE_DATA_DIR"
elif run_logged "build coverage binary" make build-cover; then
  export GOCOVERDIR="$COVERAGE_DATA_DIR"
  COVERAGE_ENABLED=1
  run_logged "coverage binary instrumentation" "$BINARY" version
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  ensure_kubernetes_source
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  run_logged "build upstream kubectl e2e binaries" build_upstream_binaries
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  UPSTREAM_KUBECTL_BIN="$(find_upstream_binary kubectl)"
  E2E_BIN="$(find_upstream_binary e2e.test)"
  if [[ ! -x "$UPSTREAM_KUBECTL_BIN" || ! -x "$E2E_BIN" ]]; then
    add_result "FAIL" "locate upstream binaries" "kubectl=${UPSTREAM_KUBECTL_BIN:-missing}, e2e.test=${E2E_BIN:-missing}"
  else
    add_result "PASS" "locate upstream binaries" "kubectl and e2e.test"
  fi
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  ensure_cluster
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  run_logged "add single-source proxy context" "$BINARY" add-context "$PROXY_CONTEXT" \
    --kubeconfig "$KUBECONFIG_FILE" \
    --state "$STATE_FILE" \
    --contexts "$SOURCE_CONTEXT" \
    --primary-context "$SOURCE_CONTEXT" \
    --listen 127.0.0.1:0 \
    --proxy-ttl 0 \
    --request-timeout 0 \
    --logs-enabled \
    --exec-command "$BINARY"
fi
if [[ "$HAD_FAILURE" -eq 0 && "$COVERAGE_ENABLED" == "1" ]]; then
  if start_coverage_proxy; then
    add_result "PASS" "start coverage proxy" "ok"
  else
    add_result "FAIL" "start coverage proxy" "could not start $STATE_FILE"
  fi
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  if write_kubectl_wrapper; then
    add_result "PASS" "configure upstream kubectl wrapper" "all e2e kubectl commands use $PROXY_CONTEXT"
  else
    add_result "FAIL" "configure upstream kubectl wrapper" "could not create $KUBECTL_WRAPPER"
  fi
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  mkdir -p "$REPORT_DIR"
  run_streamed "upstream kubectl e2e through proxy" run_upstream_kubectl_e2e
fi
if [[ "$HAD_FAILURE" -eq 0 ]]; then
  check_proxy_log
fi

if [[ "$HAD_FAILURE" -ne 0 ]]; then
  exit 1
fi
