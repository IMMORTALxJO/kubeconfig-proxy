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
  local named_watch_log="$TMP_DIR/named-configmap-watch.log"
  local paginated_watch_log="$TMP_DIR/paginated-configmap-watch.log"
  local watch_pid=""
  local named_watch_pid=""
  local paginated_watch_pid=""
  local watch_a
  local watch_b
  local named_watch
  local paginated_seed_a
  local paginated_seed_b
  local paginated_watch
  local paginated_label="kubeconfig-proxy.io/e2e-paginated-watch=$E2E_RESOURCE_PREFIX"
  local paginated_list
  local paginated_resource_version
  local source_a_resource_version
  local source_b_resource_version
  local attempt

  watch_a="$(e2e_resource_name watch-a)"
  watch_b="$(e2e_resource_name watch-b)"
  named_watch="$(e2e_resource_name watch-named)"
  paginated_seed_a="$(e2e_resource_name page-watch-seed-a)"
  paginated_seed_b="$(e2e_resource_name page-watch-seed-b)"
  paginated_watch="$(e2e_resource_name page-watch-event)"

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

  run_cmd "create named watched configmap in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" create configmap "$named_watch" --from-literal=value=before

  kubectl_ctx "$PROXY_CONTEXT" get --raw "/api/v1/namespaces/$NS/configmaps?watch=true&fieldSelector=metadata.name%3D$named_watch" >"$named_watch_log" 2>&1 &
  named_watch_pid=$!

  if wait_for_watch_pattern '"type":"ADDED"' "$named_watch_log"; then
    add_result "PASS" "named watch receives kubeconfig-proxy-a initial event" "$named_watch"
  else
    add_result "FAIL" "named watch receives kubeconfig-proxy-a initial event" "$(tail -n 20 "$named_watch_log" 2>/dev/null || true)"
    kill "$named_watch_pid" 2>/dev/null || true
    wait "$named_watch_pid" 2>/dev/null || true
    rm -f "$named_watch_log"
    return
  fi

  run_cmd "modify named watched configmap in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" label configmap "$named_watch" kubeconfig-proxy.io/e2e-named-watch=updated --overwrite
  if wait_for_watch_pattern '"type":"MODIFIED"' "$named_watch_log"; then
    add_result "PASS" "named watch receives kubeconfig-proxy-a modification" "$named_watch"
  else
    add_result "FAIL" "named watch receives kubeconfig-proxy-a modification" "$(tail -n 20 "$named_watch_log" 2>/dev/null || true)"
  fi

  kill "$named_watch_pid" 2>/dev/null || true
  wait "$named_watch_pid" 2>/dev/null || true
  rm -f "$named_watch_log"

  run_cmd "seed paginated watch configmap in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" create configmap "$paginated_seed_a" --from-literal=value=a
  run_cmd "label paginated watch configmap in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" label configmap "$paginated_seed_a" "$paginated_label"
  run_cmd "seed paginated watch configmap in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" create configmap "$paginated_seed_b" --from-literal=value=b
  run_cmd "label paginated watch configmap in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" label configmap "$paginated_seed_b" "$paginated_label"

  source_a_resource_version="$(kubectl_ctx "$CTX_A" get --raw "/api/v1/namespaces/$NS/configmaps?labelSelector=kubeconfig-proxy.io%2Fe2e-paginated-watch%3D$E2E_RESOURCE_PREFIX&limit=1&resourceVersion=0" | sed -n 's/.*"metadata":{"resourceVersion":"\([^"]*\)".*/\1/p')"
  source_b_resource_version="$(kubectl_ctx "$CTX_B" get --raw "/api/v1/namespaces/$NS/configmaps?labelSelector=kubeconfig-proxy.io%2Fe2e-paginated-watch%3D$E2E_RESOURCE_PREFIX&limit=1&resourceVersion=0" | sed -n 's/.*"metadata":{"resourceVersion":"\([^"]*\)".*/\1/p')"
  if [[ ! "$source_a_resource_version" =~ ^[0-9]+$ || ! "$source_b_resource_version" =~ ^[0-9]+$ ]]; then
    add_result "FAIL" "prepare paginated watch resource versions" "kubeconfig-proxy-a: $source_a_resource_version; kubeconfig-proxy-b: $source_b_resource_version"
    return
  fi
  for ((attempt = 1; attempt <= 200 && source_b_resource_version <= source_a_resource_version + 10; attempt++)); do
    kubectl_ctx "$CTX_B" -n "$NS" annotate configmap "$paginated_seed_b" kubeconfig-proxy.io/e2e-paginated-watch-revision="$attempt" --overwrite >/dev/null
    source_b_resource_version="$(kubectl_ctx "$CTX_B" get --raw "/api/v1/namespaces/$NS/configmaps?labelSelector=kubeconfig-proxy.io%2Fe2e-paginated-watch%3D$E2E_RESOURCE_PREFIX&limit=1&resourceVersion=0" | sed -n 's/.*"metadata":{"resourceVersion":"\([^"]*\)".*/\1/p')"
  done
  if ((source_b_resource_version <= source_a_resource_version + 10)); then
    add_result "FAIL" "prepare paginated watch resource versions" "kubeconfig-proxy-a: $source_a_resource_version; kubeconfig-proxy-b: $source_b_resource_version"
    return
  fi

  paginated_list="$(kubectl_ctx "$PROXY_CONTEXT" get --raw "/api/v1/namespaces/$NS/configmaps?labelSelector=kubeconfig-proxy.io%2Fe2e-paginated-watch%3D$E2E_RESOURCE_PREFIX&limit=500&resourceVersion=0" 2>&1)"
  paginated_resource_version="$(printf '%s' "$paginated_list" | sed -n 's/.*"metadata":{"resourceVersion":"\([^"]*\)".*/\1/p')"
  if [[ "$paginated_resource_version" == kubeconfig-proxy:* ]]; then
    add_result "PASS" "paginated list returns aggregate resource version for watch" "$paginated_resource_version"
  else
    add_result "FAIL" "paginated list returns aggregate resource version for watch" "$paginated_list"
  fi

  kubectl_ctx "$PROXY_CONTEXT" get --raw "/api/v1/namespaces/$NS/configmaps?watch=true&resourceVersion=$paginated_resource_version" >"$paginated_watch_log" 2>&1 &
  paginated_watch_pid=$!

  run_cmd "create paginated watched configmap in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" create configmap "$paginated_watch" --from-literal=value=event
  if wait_for_watch_pattern "\"name\":\"$paginated_watch\"" "$paginated_watch_log"; then
    add_result "PASS" "paginated watch receives kubeconfig-proxy-a event" "$paginated_watch"
  else
    add_result "FAIL" "paginated watch receives kubeconfig-proxy-a event" "$(tail -n 20 "$paginated_watch_log" 2>/dev/null || true)"
  fi

  kill "$paginated_watch_pid" 2>/dev/null || true
  wait "$paginated_watch_pid" 2>/dev/null || true
  rm -f "$paginated_watch_log"
}
