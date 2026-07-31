#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

# Invoked indirectly through run_cmd.
# shellcheck disable=SC2329
apply_subresource_pod() {
  local context="$1"
  local pod_name="$2"
  local logs_selector_value="$3"

  kubectl_ctx "$context" -n "$NS" apply -f - <<EOF_POD
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  labels:
    kcp-e2e-logs: $logs_selector_value
spec:
  restartPolicy: Never
  containers:
    - name: web
      image: busybox:1.37
      command:
        - sh
        - -c
        - mkdir -p /www; echo web-from-$context > /www/index.html; while true; do echo logs-from-$context; sleep 1; done & httpd -f -p 8080 -h /www
    - name: attach
      image: busybox:1.37
      command: ["sh", "-c", "while true; do echo attach-from-$context; sleep 1; done"]
EOF_POD
}

wait_for_file_pattern() {
  local pattern="$1"
  local file="$2"
  wait_for_file_pattern_count "$pattern" "$file" 1
}

wait_for_file_pattern_count() {
  local pattern="$1"
  local file="$2"
  local expected_count="$3"
  local attempt
  local match_count

  for ((attempt = 0; attempt < 50; attempt++)); do
    match_count="$(grep -F -c -- "$pattern" "$file" 2>/dev/null || true)"
    if (( match_count >= expected_count )); then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

run_subresource_checks() {
  local attach_pid=""
  local logs_selector_pid=""
  local port_forward_pid=""
  local attach_log="$TMP_DIR/subresource-attach.log"
  local logs_selector_log="$TMP_DIR/subresource-selector-logs.log"
  local port_forward_log="$TMP_DIR/subresource-port-forward.log"
  local port=$((20000 + ($$ % 10000)))
  local logs_output
  local logs_selector_value
  local exec_output
  local port_forward_output=""
  local pid
  local pod_name_a
  local pod_name

  pod_name_a="$(e2e_resource_name subresource-pod-a)"
  pod_name="$(e2e_resource_name subresource-pod-b)"
  logs_selector_value="$E2E_RESOURCE_PREFIX"

  run_cmd "seed pod for pod subresources in kubeconfig-proxy-a" apply_subresource_pod "$CTX_A" "$pod_name_a" "$logs_selector_value"
  run_cmd "seed pod for pod subresources in kubeconfig-proxy-b" apply_subresource_pod "$CTX_B" "$pod_name" "$logs_selector_value"
  run_cmd "wait for kubeconfig-proxy-a pod subresources readiness" kubectl_ctx "$CTX_A" -n "$NS" wait --for=condition=Ready "pod/$pod_name_a" --timeout=90s
  run_cmd "wait for kubeconfig-proxy-b pod subresources readiness" kubectl_ctx "$CTX_B" -n "$NS" wait --for=condition=Ready "pod/$pod_name" --timeout=90s
  expect_not_found "subresource pod absent from kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get pod "$pod_name"

  logs_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" logs "$pod_name" -c web 2>&1)"
  if [[ "$logs_output" == *"logs-from-$CTX_B"* ]]; then
    add_result "PASS" "kubectl logs routes to cluster containing pod" "read kubeconfig-proxy-b pod logs"
  else
    add_result "FAIL" "kubectl logs routes to cluster containing pod" "$logs_output"
  fi

  kubectl_ctx "$PROXY_CONTEXT" -n "$NS" logs --selector="kcp-e2e-logs=$logs_selector_value" --follow --tail=1 --prefix -c web >"$logs_selector_log" 2>&1 &
  logs_selector_pid=$!
  if wait_for_file_pattern_count "logs-from-$CTX_A" "$logs_selector_log" 3 && wait_for_file_pattern_count "logs-from-$CTX_B" "$logs_selector_log" 3; then
    add_result "PASS" "kubectl logs --selector streams pods from both contexts" "received at least three followed logs from each source context"
  else
    add_result "FAIL" "kubectl logs --selector streams pods from both contexts" "$(tail -n 20 "$logs_selector_log" 2>/dev/null || true)"
  fi

  exec_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" exec "$pod_name" -c web -- cat /www/index.html 2>&1)"
  if [[ "$exec_output" == *"web-from-$CTX_B"* ]]; then
    add_result "PASS" "kubectl exec routes to cluster containing pod" "executed command in kubeconfig-proxy-b pod"
  else
    add_result "FAIL" "kubectl exec routes to cluster containing pod" "$exec_output"
  fi

  kubectl_ctx "$PROXY_CONTEXT" -n "$NS" attach "$pod_name" -c attach >"$attach_log" 2>&1 &
  attach_pid=$!
  if wait_for_file_pattern "attach-from-$CTX_B" "$attach_log"; then
    add_result "PASS" "kubectl attach routes to cluster containing pod" "received kubeconfig-proxy-b container output"
  else
    add_result "FAIL" "kubectl attach routes to cluster containing pod" "$(tail -n 20 "$attach_log" 2>/dev/null || true)"
  fi

  kubectl_ctx "$PROXY_CONTEXT" -n "$NS" port-forward "pod/$pod_name" "$port:8080" >"$port_forward_log" 2>&1 &
  port_forward_pid=$!
  for ((pid = 0; pid < 50; pid++)); do
    if port_forward_output="$(curl --fail --silent --show-error --max-time 1 "http://127.0.0.1:$port" 2>/dev/null)"; then
      break
    fi
    sleep 0.1
  done
  if [[ "$port_forward_output" == *"web-from-$CTX_B"* ]]; then
    add_result "PASS" "kubectl port-forward routes to cluster containing pod" "served kubeconfig-proxy-b pod response"
  else
    add_result "FAIL" "kubectl port-forward routes to cluster containing pod" "$(tail -n 20 "$port_forward_log" 2>/dev/null || true)"
  fi

  for pid in "$logs_selector_pid" "$attach_pid" "$port_forward_pid"; do
    [[ -n "$pid" ]] || continue
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -f "$attach_log" "$logs_selector_log" "$port_forward_log"
}
