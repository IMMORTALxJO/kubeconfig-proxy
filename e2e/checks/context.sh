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
}
