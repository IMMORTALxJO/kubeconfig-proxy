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
CTX_A="kind-kubeconfig-proxy-a"
CTX_B="kind-kubeconfig-proxy-b"
NS="default"
WERF_NS="${KCP_WERF_NAMESPACE:-kcp-werf-$$}"
TIMEOUT="${KCP_TEST_TIMEOUT:-30s}"
BINARY="$ROOT/bin/kubeconfig-proxy"

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

print_results() {
  printf '\n| Status | Check | Details |\n'
  printf '| --- | --- | --- |\n'
  local i
  for i in "${!RESULT_STATUS[@]}"; do
    printf '| %s | %s | %s |\n' "$(format_status "${RESULT_STATUS[$i]}")" "${RESULT_NAME[$i]}" "${RESULT_DETAILS[$i]}"
  done
}

cleanup() {
  local code=$?
  if [[ "${KCP_KEEP_KIND:-0}" == "1" ]]; then
    add_result "SKIP" "cleanup" "KCP_KEEP_KIND=1, leaving $TMP_DIR and kind clusters"
    print_results
    exit "$code"
  fi

  if [[ -x "$BINARY" && -f "$KUBECONFIG_FILE" ]]; then
    KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
    KUBECONFIG="$KUBECONFIG_FILE" "$BINARY" delete-context "$RO_PROXY_CONTEXT" --kubeconfig "$KUBECONFIG_FILE" >/dev/null 2>&1 || true
  fi

  if [[ -f "$KUBECONFIG_FILE" ]]; then
    KUBECONFIG="$KUBECONFIG_FILE" kubectl --request-timeout="$TIMEOUT" --context "$CTX_A" delete namespace "$WERF_NS" --ignore-not-found >/dev/null 2>&1 || true
    KUBECONFIG="$KUBECONFIG_FILE" kubectl --request-timeout="$TIMEOUT" --context "$CTX_B" delete namespace "$WERF_NS" --ignore-not-found >/dev/null 2>&1 || true
  fi

  local cluster
  if [[ "${#CREATED_CLUSTERS[@]}" -gt 0 ]]; then
    for cluster in "${CREATED_CLUSTERS[@]}"; do
      kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
    done
  fi
  rm -rf "$TMP_DIR"
  print_results
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
  KUBECONFIG="$KUBECONFIG_FILE" kubectl --request-timeout="$TIMEOUT" "$@"
}

kubectl_ctx() {
  local ctx="$1"
  shift
  kubectl_cmd --context "$ctx" "$@"
}

expect_contains() {
  local name="$1"
  local haystack="$2"
  local needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    add_result "PASS" "$name" "found $needle"
    return 0
  fi
  add_result "FAIL" "$name" "missing $needle in: $haystack"
  return 1
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

ensure_cluster() {
  local cluster="$1"
  if cluster_exists "$cluster"; then
    if [[ "${KCP_RECREATE_KIND:-0}" == "1" ]]; then
      run_cmd "delete existing kind cluster $cluster" kind delete cluster --name "$cluster" || return 1
    else
      run_cmd "export existing kind cluster $cluster" kind export kubeconfig --name "$cluster" --kubeconfig "$KUBECONFIG_FILE" || return 1
      add_result "PASS" "kind cluster $cluster" "reused existing cluster"
      return 0
    fi
  fi

  if run_cmd "create kind cluster $cluster" kind create cluster --name "$cluster" --kubeconfig "$KUBECONFIG_FILE"; then
    CREATED_CLUSTERS+=("$cluster")
    return 0
  fi
  return 1
}

cleanup_test_resources() {
  local ctx
  for ctx in "$CTX_A" "$CTX_B"; do
    kubectl_ctx "$ctx" -n "$NS" delete pod kcp-subresource-pod --ignore-not-found >/dev/null 2>&1 || true
    kubectl_ctx "$ctx" -n "$NS" delete configmap \
    kcp-only-a kcp-only-b kcp-fanout kcp-target-b kcp-single kcp-delete-a kcp-readonly \
      --ignore-not-found >/dev/null 2>&1 || true
    kubectl_ctx "$ctx" -n "$NS" delete secret sh.helm.release.v1.kcp.v1 --ignore-not-found >/dev/null 2>&1 || true
  done
}

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
  if [[ -x "$BINARY" ]]; then
    add_result "SKIP" "make check" "KCP_SKIP_MAKE_CHECK=1, using $BINARY"
  else
    add_result "FAIL" "make check" "KCP_SKIP_MAKE_CHECK=1 but $BINARY is missing or not executable"
  fi
else
  run_cmd "make check" make check
fi

require_cmd kind
require_cmd kubectl
if [[ "${KCP_SKIP_WERF:-0}" == "1" ]]; then
  add_result "SKIP" "required tool: werf" "KCP_SKIP_WERF=1"
else
  require_cmd werf
fi

if [[ ! -x "$BINARY" ]]; then
  add_result "FAIL" "binary from make check" "$BINARY is missing or not executable"
else
  add_result "PASS" "binary from make check" "$BINARY"
fi

if [[ "$HAD_FAILURE" -ne 0 ]]; then
  add_result "SKIP" "integration checks" "previous setup check failed"
  exit 1
fi

ensure_cluster kubeconfig-proxy-a
ensure_cluster kubeconfig-proxy-b

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

run_cmd "proxy discovery and exec credential auto-start" kubectl_ctx "$PROXY_CONTEXT" version

run_cmd "seed aggregate resources in source clusters" bash -c "
  set -euo pipefail
  KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create configmap kcp-only-a --from-literal=value=a
  KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create configmap kcp-only-b --from-literal=value=b
"

aggregate_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.metadata.labels.context}{"\n"}{end}' 2>&1)"
if [[ "$aggregate_output" == *"kcp-only-a=$CTX_A"* && "$aggregate_output" == *"kcp-only-b=$CTX_B"* ]]; then
  add_result "PASS" "aggregated list adds context labels" "saw kcp-only-a=$CTX_A and kcp-only-b=$CTX_B"
else
  add_result "FAIL" "aggregated list adds context labels" "$aggregate_output"
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

run_cmd "seed pod for logs and exec in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" run kcp-subresource-pod \
  --image=busybox:1.37 \
  --restart=Never \
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
  KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' create secret generic sh.helm.release.v1.kcp.v1 --from-literal=release=a --dry-run=client -o yaml | KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' apply -f -
  KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_A' -n '$NS' label secret sh.helm.release.v1.kcp.v1 owner=helm name=kcp --overwrite
  KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' create secret generic sh.helm.release.v1.kcp.v1 --from-literal=release=b --dry-run=client -o yaml | KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' apply -f -
  KUBECONFIG='$KUBECONFIG_FILE' kubectl --request-timeout='$TIMEOUT' --context '$CTX_B' -n '$NS' label secret sh.helm.release.v1.kcp.v1 owner=helm name=kcp --overwrite
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
    KUBECONFIG='$KUBECONFIG_FILE' werf converge --env kind --dev --namespace '$WERF_NS' --kube-context '$PROXY_CONTEXT'
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
