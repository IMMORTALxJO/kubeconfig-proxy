#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

# Invoked indirectly through run_cmd.
# shellcheck disable=SC2329
apply_subresource_pod() {
  local pod_name="$1"

  kubectl_ctx "$CTX_B" -n "$NS" apply -f - <<EOF_POD
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
spec:
  restartPolicy: Never
  containers:
    - name: web
      image: busybox:1.37
      command:
        - sh
        - -c
        - mkdir -p /www; echo web-from-$CTX_B > /www/index.html; echo logs-from-$CTX_B; httpd -f -p 8080 -h /www
    - name: attach
      image: busybox:1.37
      command: ["sh", "-c", "while true; do echo attach-from-$CTX_B; sleep 1; done"]
EOF_POD
}

wait_for_file_pattern() {
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

run_subresource_checks() {
  local attach_pid=""
  local port_forward_pid=""
  local attach_log="$TMP_DIR/subresource-attach.log"
  local port_forward_log="$TMP_DIR/subresource-port-forward.log"
  local port=$((20000 + ($$ % 10000)))
  local logs_output
  local exec_output
  local port_forward_output=""
  local pid
  local pod_name

  pod_name="$(e2e_resource_name subresource-pod)"

  run_cmd "seed pod for pod subresources in kubeconfig-proxy-b" apply_subresource_pod "$pod_name"
  run_cmd "wait for pod subresources readiness" kubectl_ctx "$CTX_B" -n "$NS" wait --for=condition=Ready "pod/$pod_name" --timeout=90s
  expect_not_found "subresource pod absent from kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get pod "$pod_name"

  logs_output="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" logs "$pod_name" -c web 2>&1)"
  if [[ "$logs_output" == *"logs-from-$CTX_B"* ]]; then
    add_result "PASS" "kubectl logs routes to cluster containing pod" "read kubeconfig-proxy-b pod logs"
  else
    add_result "FAIL" "kubectl logs routes to cluster containing pod" "$logs_output"
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

  for pid in "$attach_pid" "$port_forward_pid"; do
    [[ -n "$pid" ]] || continue
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -f "$attach_log" "$port_forward_log"
}
