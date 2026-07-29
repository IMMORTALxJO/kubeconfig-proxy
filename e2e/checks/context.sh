#!/usr/bin/env bash

# Sourced by e2e/run.sh after the proxy contexts have been created.

run_context_checks() {
  local duplicate_output
  local duplicate_status
  local state_file_count

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
}
