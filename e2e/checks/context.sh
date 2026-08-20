#!/usr/bin/env bash

# Sourced by e2e/run.sh after the proxy contexts have been created.

run_context_checks() {
  local absolute_kubeconfig
  local duplicate_output
  local duplicate_status
  local dynamic_kubeconfig
  local moved_kubeconfig
  local state_file_count
  local stored_kubeconfig

  absolute_kubeconfig="$(printf '%s\n' "$KUBECONFIG_FILE" | sed 's#//*#/#g')"
  stored_kubeconfig="$(sed -n 's/^sourceKubeconfig: //p' "$STATE_FILE")"
  if [[ "$stored_kubeconfig" == "$absolute_kubeconfig" ]]; then
    add_result "PASS" "explicit kubeconfig path is stored in state" "$absolute_kubeconfig"
  else
    add_result "FAIL" "explicit kubeconfig path is stored in state" "stored=$stored_kubeconfig expected=$absolute_kubeconfig"
  fi

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
    --logs-enabled \
    --exec-command "$BINARY"
  run_cmd "add proxy context with safe default state path" env HOME="$TMP_DIR/home" "$BINARY" add-context "$SAFE_PROXY_CONTEXT" \
    --kubeconfig "$KUBECONFIG_FILE" \
    --contexts "$CTX_A,$CTX_B" \
    --listen "127.0.0.1:0" \
    --logs-enabled \
    --exec-command "$BINARY"
  state_file_count="$(find "$TMP_DIR/home/.kube/kubeconfig-proxy" -type f -name '*.yaml' | wc -l | tr -d ' ')"
  if [[ "$state_file_count" == "2" ]]; then
    add_result "PASS" "default state paths avoid sanitized-name collisions" "created two distinct state files"
  else
    add_result "FAIL" "default state paths avoid sanitized-name collisions" "found $state_file_count state files"
  fi

  dynamic_kubeconfig="$TMP_DIR/dynamic-kubeconfig"
  moved_kubeconfig="$TMP_DIR/moved-kubeconfig"
  cp "$KUBECONFIG_FILE" "$dynamic_kubeconfig"
  run_cmd "add proxy context with default kubeconfig loading" env KUBECONFIG="$dynamic_kubeconfig" "$BINARY" add-context "$DYNAMIC_PROXY_CONTEXT" \
    --state "$DYNAMIC_STATE_FILE" \
    --contexts "$CTX_A,$CTX_B" \
    --primary-context "$CTX_A" \
    --listen "127.0.0.1:0" \
    --proxy-ttl "2m" \
    --request-timeout "$TIMEOUT" \
    --logs-enabled \
    --exec-command "$BINARY"
  if grep -q '^sourceKubeconfig:' "$DYNAMIC_STATE_FILE"; then
    add_result "FAIL" "default kubeconfig path is omitted from state" "unexpected sourceKubeconfig key"
  else
    add_result "PASS" "default kubeconfig path is omitted from state" "state uses standard loading rules"
  fi
  mv "$dynamic_kubeconfig" "$moved_kubeconfig"
  run_cmd "proxy follows default kubeconfig after file move" env KUBECONFIG="$moved_kubeconfig" "$KUBECTL_BIN" \
    --request-timeout="$TIMEOUT" \
    --context "$DYNAMIC_PROXY_CONTEXT" \
    get nodes

  run_many_context_startup_check
}

run_many_context_startup_check() {
  local context_alias
  local contexts_csv=""
  local elapsed
  local i
  local output
  local proxy_server
  local reload_pending=0
  local source_cluster
  local source_user
  local started_at

  if ! source_cluster="$(kubectl_cmd config view --raw -o "jsonpath={.contexts[?(@.name==\"$CTX_A\")].context.cluster}")"; then
    add_result "FAIL" "resolve kind cluster for startup aliases" "could not read cluster from $CTX_A"
    return 1
  fi
  if ! source_user="$(kubectl_cmd config view --raw -o "jsonpath={.contexts[?(@.name==\"$CTX_A\")].context.user}")"; then
    add_result "FAIL" "resolve kind user for startup aliases" "could not read user from $CTX_A"
    return 1
  fi
  if [[ -z "$source_cluster" || -z "$source_user" ]]; then
    add_result "FAIL" "resolve kind credentials for startup aliases" "cluster=$source_cluster user=$source_user"
    return 1
  fi

  for ((i = 1; i <= 20; i++)); do
    context_alias="${CTX_A}-startup-${i}"
    if ! kubectl_cmd config set-context "$context_alias" --cluster="$source_cluster" --user="$source_user" >/dev/null; then
      add_result "FAIL" "create 20 aliases for one kind context" "could not create $context_alias"
      return 1
    fi
    contexts_csv="${contexts_csv:+$contexts_csv,}$context_alias"
  done
  add_result "PASS" "create 20 aliases for one kind context" "$CTX_A"

  run_cmd "add proxy context with 20 kind aliases" "$BINARY" add-context "$MANY_CONTEXTS_PROXY_CONTEXT" \
    --kubeconfig "$KUBECONFIG_FILE" \
    --state "$MANY_CONTEXTS_STATE_FILE" \
    --contexts "$contexts_csv" \
    --primary-context "${CTX_A}-startup-1" \
    --listen "127.0.0.1:0" \
    --proxy-ttl "2m" \
    --request-timeout "$TIMEOUT" \
    --logs-enabled \
    --exec-command "$BINARY" || return 1

  started_at="$(date +%s)"
  if output="$(kubectl_ctx "$MANY_CONTEXTS_PROXY_CONTEXT" get nodes -o name 2>&1)"; then
    elapsed=$(( $(date +%s) - started_at ))
    add_result "PASS" "first request starts proxy with 20 kind aliases" "ready after ${elapsed}s"
    check_proxy_log "$MANY_CONTEXTS_STATE_FILE"
  else
    elapsed=$(( $(date +%s) - started_at ))
    add_result "FAIL" "first request starts proxy with 20 kind aliases" "failed after ${elapsed}s: $output"
    return 1
  fi

  if ! kubectl_cmd config set-context "${CTX_A}-startup-1" --namespace=default >/dev/null; then
    add_result "FAIL" "update source kubeconfig during proxy activity" "could not update startup alias"
    return 1
  fi
  if ! proxy_server="$(kubectl_cmd config view --minify --context "$MANY_CONTEXTS_PROXY_CONTEXT" -o 'jsonpath={.clusters[0].cluster.server}')"; then
    add_result "FAIL" "resolve proxy listener for reload availability check" "could not read proxy server"
    return 1
  fi
  if output="$(kubectl_ctx "$MANY_CONTEXTS_PROXY_CONTEXT" get nodes -o name 2>&1)"; then
    add_result "PASS" "second sequential request survives source kubeconfig update" "request completed after the source kubeconfig update"
  else
    add_result "FAIL" "second sequential request survives source kubeconfig update" "$output"
    return 1
  fi

  # Any HTTP response is sufficient here; the loop is checking that the local listener never disappears.
  for ((i = 1; i <= 400; i++)); do
    if ! output="$(curl --connect-timeout 0.2 --max-time 1 --silent --show-error --insecure --output /dev/null "$proxy_server/version" 2>&1)"; then
      add_result "FAIL" "local proxy listener stays available during runtime reload" "request $i failed: $output"
      return 1
    fi
  done
  for ((i = 1; i <= 80; i++)); do
    if grep -Eq "runtime configuration changed, waiting for|source kubeconfig changed, replacing serve process" "${MANY_CONTEXTS_STATE_FILE}.log"; then
      reload_pending=1
      break
    fi
    sleep 0.05
  done
  if [[ "$reload_pending" != "1" ]]; then
    add_result "FAIL" "wait for pending runtime reload between sequential requests" "watcher did not detect the source kubeconfig update"
    return 1
  fi
  add_result "PASS" "local proxy listener stays available during runtime reload" "400 requests completed while the watcher detected the source kubeconfig update"
}
