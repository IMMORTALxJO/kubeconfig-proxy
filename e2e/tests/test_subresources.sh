#!/bin/bash

set -euo pipefail
source "$(dirname "$0")/_libs.sh"

r="$(test_name subresources)"
pod="$(test_name subresources-pod)"
port=$((20000 + (TS % 10000)))
attach_pid=""
port_forward_pid=""
attach_log="${T}.attach"
port_forward_log="${T}.port-forward"

cleanup() {
    local status=$?

    for pid in "$attach_pid" "$port_forward_pid"; do
        [[ -n "$pid" ]] || continue
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    delete_test_namespace "$r"
    rm -f "$attach_log"
    rm -f "$port_forward_log"
    cleanup_test_file
    exit "$status"
}
trap cleanup EXIT INT TERM

create_test_namespace "$r"
k_b -n "$r" apply -f - <<EOF_POD
apiVersion: v1
kind: Pod
metadata:
  name: $pod
spec:
  restartPolicy: Never
  containers:
    - name: web
      image: busybox:1.37
      command:
        - sh
        - -c
        - mkdir -p /www; echo web-from-$CONTEXT_B > /www/index.html; echo logs-from-$CONTEXT_B; httpd -f -p 8080 -h /www
    - name: attach
      image: busybox:1.37
      command: ["sh", "-c", "while true; do echo attach-from-$CONTEXT_B; sleep 1; done"]
EOF_POD
wait_for_pod_ready k_b "$pod" "$r"
ne_a "pod/$pod" "$r"

logs_output="$(k_p -n "$r" logs "$pod" -c web)"
assert_contains "$logs_output" "logs-from-$CONTEXT_B"

exec_output="$(k_p -n "$r" exec "$pod" -c web -- cat /www/index.html)"
assert_contains "$exec_output" "web-from-$CONTEXT_B"

k_p -n "$r" attach "$pod" -c attach >"$attach_log" 2>&1 &
attach_pid=$!
for ((attempt = 0; attempt < 50; attempt++)); do
    if grep -q "attach-from-$CONTEXT_B" "$attach_log"; then
        break
    fi
    sleep 0.1
done
assert_contains "$(cat "$attach_log")" "attach-from-$CONTEXT_B"

k_p -n "$r" port-forward "pod/$pod" "$port:8080" >"$port_forward_log" 2>&1 &
port_forward_pid=$!
for ((attempt = 0; attempt < 50; attempt++)); do
    if port_forward_output="$(curl --fail --silent --show-error --max-time 1 "http://127.0.0.1:$port" 2>/dev/null)"; then
        break
    fi
    sleep 0.1
done
assert_contains "${port_forward_output:-}" "web-from-$CONTEXT_B"
