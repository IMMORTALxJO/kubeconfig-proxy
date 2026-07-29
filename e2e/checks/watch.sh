#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

wait_for_watch_pattern() {
  local pattern="$1"
  local file="$2"
  local attempt

  for ((attempt = 0; attempt < 50; attempt++)); do
    if grep -q "$pattern" "$file" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

run_watch_checks() {
  local watch_log="$TMP_DIR/configmap-watch.log"
  local watch_pid=""
  local watch_a="kcp-watch-a"
  local watch_b="kcp-watch-b"

  kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmaps -w >"$watch_log" 2>&1 &
  watch_pid=$!

  run_cmd "create watched configmap in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" create configmap "$watch_a" --from-literal=value=a
  if wait_for_watch_pattern "$watch_a" "$watch_log"; then
    add_result "PASS" "watch receives kubeconfig-proxy-a event" "$watch_a"
  else
    add_result "FAIL" "watch receives kubeconfig-proxy-a event" "$(tail -n 20 "$watch_log" 2>/dev/null || true)"
  fi

  run_cmd "create watched configmap in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" create configmap "$watch_b" --from-literal=value=b
  if wait_for_watch_pattern "$watch_b" "$watch_log"; then
    add_result "PASS" "watch receives kubeconfig-proxy-b event" "$watch_b"
  else
    add_result "FAIL" "watch receives kubeconfig-proxy-b event" "$(tail -n 20 "$watch_log" 2>/dev/null || true)"
  fi

  kill "$watch_pid" 2>/dev/null || true
  wait "$watch_pid" 2>/dev/null || true
  rm -f "$watch_log"
}
