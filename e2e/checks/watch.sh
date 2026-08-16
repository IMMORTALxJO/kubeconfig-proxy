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

watch_event_resource_version() {
  local name="$1"
  local file="$2"

  grep -F "\"name\":\"$name\"" "$file" 2>/dev/null |
    tail -n 1 |
    sed -n 's/.*"resourceVersion":"\([^"]*\)".*/\1/p'
}

decode_aggregate_resource_version() {
  local value="$1"
  local encoded

  [[ "$value" == kubeconfig-proxy:* ]] || return 1
  encoded="${value#kubeconfig-proxy:}"
  encoded="${encoded//-/+}"
  encoded="${encoded//_/\/}"
  case $((${#encoded} % 4)) in
    2) encoded="${encoded}==" ;;
    3) encoded="${encoded}=" ;;
    0) ;;
    *) return 1 ;;
  esac

  if base64 --decode </dev/null >/dev/null 2>&1; then
    printf '%s' "$encoded" | base64 --decode
  else
    printf '%s' "$encoded" | base64 -D
  fi
}

encode_aggregate_resource_versions() {
  local value="$1"
  local encoded

  encoded="$(printf '%s' "$value" | base64 | tr -d '\n' | tr '+/' '-_' | tr -d '=')"
  printf 'kubeconfig-proxy:%s' "$encoded"
}

aggregate_context_resource_version() {
  local value="$1"
  local context_name="$2"

  printf '%s' "$value" | sed -n "s/.*\"$context_name\":\"\([^\"]*\)\".*/\1/p"
}

compact_context_etcd_history() {
  local context_name="$1"
  local etcd_pod
  local status
  local revision
  local -a etcdctl=(
    etcdctl
    --endpoints=https://127.0.0.1:2379
    --cacert=/etc/kubernetes/pki/etcd/ca.crt
    --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt
    --key=/etc/kubernetes/pki/etcd/healthcheck-client.key
  )

  etcd_pod="$(kubectl_ctx "$context_name" -n kube-system get pods -l component=etcd -o jsonpath='{.items[0].metadata.name}')" || return
  [[ -n "$etcd_pod" ]] || return 1
  status="$(kubectl_ctx "$context_name" -n kube-system exec "$etcd_pod" -- "${etcdctl[@]}" endpoint status --write-out=json)" || return
  revision="$(printf '%s' "$status" | sed -n 's/.*"revision":\([0-9][0-9]*\).*/\1/p')"
  [[ -n "$revision" ]] || return 1
  kubectl_ctx "$context_name" -n kube-system exec "$etcd_pod" -- "${etcdctl[@]}" compact "$revision" >/dev/null
}

advance_configmap_watch_cache() {
  local context_name="$1"
  local namespace="$2"
  local configmap="$3"
  local revision

  for ((revision = 1; revision <= 256; revision++)); do
    kubectl_ctx "$context_name" -n "$namespace" annotate configmap "$configmap" kubeconfig-proxy.io/e2e-watch-cache-revision="$revision" --overwrite >/dev/null || return
  done
}

wait_for_process_exit() {
  local pid="$1"
  local attempt

  for ((attempt = 0; attempt < 20; attempt++)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid" 2>/dev/null || true
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
  local paginated_watch_b
  local paginated_label="kubeconfig-proxy.io/e2e-paginated-watch=$E2E_RESOURCE_PREFIX"
  local paginated_list
  local paginated_resource_version
  local watch_a_resource_version
  local watch_b_resource_version
  local watch_a_resource_versions
  local watch_b_resource_versions
  local watch_a_context_a_version
  local watch_a_context_b_version
  local watch_b_context_a_version
  local watch_b_context_b_version
  local dropped_watch_log="$TMP_DIR/dropped-upstream-watch.log"
  local dropped_watch_pid=""
  local dropped_watch_list
  local dropped_watch_resource_version
  local dropped_watch_resource_versions
  local dropped_watch_context_b_version
  local forced_stale_resource_version
  local source_a_resource_version
  local source_b_resource_version
  local attempt

  watch_a="$(e2e_resource_name watch-a)"
  watch_b="$(e2e_resource_name watch-b)"
  named_watch="$(e2e_resource_name watch-named)"
  paginated_seed_a="$(e2e_resource_name page-watch-seed-a)"
  paginated_seed_b="$(e2e_resource_name page-watch-seed-b)"
  paginated_watch="$(e2e_resource_name page-watch-event)"
  paginated_watch_b="$(e2e_resource_name page-watch-event-b)"

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

  watch_a_resource_version="$(watch_event_resource_version "$paginated_watch" "$paginated_watch_log")"
  if watch_a_resource_versions="$(decode_aggregate_resource_version "$watch_a_resource_version" 2>/dev/null)" &&
    [[ "$watch_a_resource_versions" == *"\"$CTX_A\":\""* && "$watch_a_resource_versions" == *"\"$CTX_B\":\""* ]]; then
    add_result "PASS" "watch event keeps aggregate resource versions after kubeconfig-proxy-a update" "$watch_a_resource_versions"
  else
    add_result "FAIL" "watch event keeps aggregate resource versions after kubeconfig-proxy-a update" "resourceVersion=$watch_a_resource_version"
  fi

  run_cmd "create paginated watched configmap in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" create configmap "$paginated_watch_b" --from-literal=value=event
  if wait_for_watch_pattern "\"name\":\"$paginated_watch_b\"" "$paginated_watch_log"; then
    add_result "PASS" "paginated watch receives kubeconfig-proxy-b event" "$paginated_watch_b"
  else
    add_result "FAIL" "paginated watch receives kubeconfig-proxy-b event" "$(tail -n 20 "$paginated_watch_log" 2>/dev/null || true)"
  fi

  watch_b_resource_version="$(watch_event_resource_version "$paginated_watch_b" "$paginated_watch_log")"
  if watch_b_resource_versions="$(decode_aggregate_resource_version "$watch_b_resource_version" 2>/dev/null)" &&
    [[ "$watch_b_resource_versions" == *"\"$CTX_A\":\""* && "$watch_b_resource_versions" == *"\"$CTX_B\":\""* ]]; then
    add_result "PASS" "watch event keeps aggregate resource versions after kubeconfig-proxy-b update" "$watch_b_resource_versions"
  else
    add_result "FAIL" "watch event keeps aggregate resource versions after kubeconfig-proxy-b update" "resourceVersion=$watch_b_resource_version"
  fi

  watch_a_context_a_version="$(aggregate_context_resource_version "$watch_a_resource_versions" "$CTX_A")"
  watch_a_context_b_version="$(aggregate_context_resource_version "$watch_a_resource_versions" "$CTX_B")"
  watch_b_context_a_version="$(aggregate_context_resource_version "$watch_b_resource_versions" "$CTX_A")"
  watch_b_context_b_version="$(aggregate_context_resource_version "$watch_b_resource_versions" "$CTX_B")"
  if [[ -n "$watch_a_context_a_version" &&
    "$watch_b_context_a_version" == "$watch_a_context_a_version" &&
    -n "$watch_a_context_b_version" &&
    -n "$watch_b_context_b_version" &&
    "$watch_b_context_b_version" != "$watch_a_context_b_version" ]]; then
    add_result "PASS" "watch advances only the event source resource version" "$watch_b_resource_versions"
  else
    add_result "FAIL" "watch advances only the event source resource version" "after-a=$watch_a_resource_versions; after-b=$watch_b_resource_versions"
  fi

  kill "$paginated_watch_pid" 2>/dev/null || true
  wait "$paginated_watch_pid" 2>/dev/null || true
  rm -f "$paginated_watch_log"

  if ! run_cmd "compact kubeconfig-proxy-a etcd history for expired watch" compact_context_etcd_history "$CTX_A"; then
    return
  fi
  if ! run_cmd "advance kubeconfig-proxy-a watch cache past compaction" advance_configmap_watch_cache "$CTX_A" "$NS" "$paginated_seed_a"; then
    return
  fi
  dropped_watch_list="$(kubectl_ctx "$PROXY_CONTEXT" get --raw "/api/v1/namespaces/$NS/configmaps?resourceVersion=0" 2>&1)"
  dropped_watch_resource_version="$(printf '%s' "$dropped_watch_list" | sed -n 's/.*"metadata":{"resourceVersion":"\([^"]*\)".*/\1/p')"
  if ! dropped_watch_resource_versions="$(decode_aggregate_resource_version "$dropped_watch_resource_version" 2>/dev/null)"; then
    add_result "FAIL" "prepare dropped upstream watch" "$dropped_watch_list"
    return
  fi
  dropped_watch_context_b_version="$(aggregate_context_resource_version "$dropped_watch_resource_versions" "$CTX_B")"
  if [[ -z "$dropped_watch_context_b_version" ]]; then
    add_result "FAIL" "prepare dropped upstream watch" "$dropped_watch_resource_versions"
    return
  fi
  forced_stale_resource_version="$(encode_aggregate_resource_versions "{\"$CTX_A\":\"1\",\"$CTX_B\":\"$dropped_watch_context_b_version\"}")"

  kubectl_ctx "$PROXY_CONTEXT" get --raw "/api/v1/namespaces/$NS/configmaps?watch=true&timeoutSeconds=20&resourceVersion=$forced_stale_resource_version" >"$dropped_watch_log" 2>&1 &
  dropped_watch_pid=$!
  if ! wait_for_watch_pattern '"type":"ERROR"' "$dropped_watch_log"; then
    add_result "FAIL" "one upstream watch terminates" "$(tail -n 20 "$dropped_watch_log" 2>/dev/null || true)"
    kill "$dropped_watch_pid" 2>/dev/null || true
    wait "$dropped_watch_pid" 2>/dev/null || true
    rm -f "$dropped_watch_log"
    return
  fi
  add_result "PASS" "one upstream watch terminates" "kubeconfig-proxy-a rejected stale resourceVersion"

  if wait_for_process_exit "$dropped_watch_pid"; then
    add_result "PASS" "aggregate watch closes when one upstream terminates" "client stream closed"
  else
    add_result "FAIL" "aggregate watch closes when one upstream terminates" "client stream remained open after kubeconfig-proxy-a watch ended; $(tail -n 20 "$dropped_watch_log" 2>/dev/null || true)"
    kill "$dropped_watch_pid" 2>/dev/null || true
    wait "$dropped_watch_pid" 2>/dev/null || true
  fi
  rm -f "$dropped_watch_log"
}
