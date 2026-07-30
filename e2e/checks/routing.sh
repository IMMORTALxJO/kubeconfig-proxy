#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

# Invoked indirectly through run_cmd.
# shellcheck disable=SC2329
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

run_routing_checks() {
  local target_b_manifest="$TMP_DIR/context-name.yaml"
  local single_manifest="$TMP_DIR/single-context.yaml"
  local named_get_value
  local patch_value
  local readonly_output
  local debug_container="debugger"
  local debug_name
  local fanout_name
  local target_b_name
  local single_name
  local delete_a_name
  local debug_pod_name
  local readonly_name

  fanout_name="$(e2e_resource_name fanout)"
  target_b_name="$(e2e_resource_name target-b)"
  single_name="$(e2e_resource_name single)"
  delete_a_name="$(e2e_resource_name delete-a)"
  debug_pod_name="$(e2e_resource_name debug-pod)"
  readonly_name="$(e2e_resource_name readonly)"

  run_cmd "fan-out create mutation" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" create configmap "$fanout_name" --from-literal=value=shared
  expect_exists "fan-out object exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap "$fanout_name"
  expect_exists "fan-out object exists in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap "$fanout_name"

  write_configmap_manifest "$target_b_manifest" "$target_b_name" "  annotations:
    kubeconfig-proxy.io/context-name: $CTX_B"
  run_cmd "context-name routes create to kubeconfig-proxy-b" apply_manifest "$PROXY_CONTEXT" "$target_b_manifest"
  expect_not_found "context-name object absent from kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap "$target_b_name"
  expect_exists "context-name object exists in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap "$target_b_name"

  named_get_value="$(kubectl_ctx "$PROXY_CONTEXT" -n "$NS" get configmap "$target_b_name" -o jsonpath='{.metadata.name}' 2>&1)"
  if [[ "$named_get_value" == "$target_b_name" ]]; then
    add_result "PASS" "named GET routes to cluster containing object" "found object through proxy"
  else
    add_result "FAIL" "named GET routes to cluster containing object" "$named_get_value"
  fi

  write_configmap_manifest "$single_manifest" "$single_name" "  annotations:
    kubeconfig-proxy.io/single-context: \"true\""
  run_cmd "single-context routes create to first context" apply_manifest "$PROXY_CONTEXT" "$single_manifest"
  expect_exists "single-context object exists in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap "$single_name"
  expect_not_found "single-context object absent from kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap "$single_name"

  run_cmd "PATCH uses existing object routing" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" patch configmap "$target_b_name" --type merge -p '{"data":{"patched":"yes"}}'
  patch_value="$(kubectl_ctx "$CTX_B" -n "$NS" get configmap "$target_b_name" -o jsonpath='{.data.patched}' 2>&1)"
  if [[ "$patch_value" == "yes" ]]; then
    add_result "PASS" "PATCH changed only annotated target" "kubeconfig-proxy-b patched"
  else
    add_result "FAIL" "PATCH changed only annotated target" "$patch_value"
  fi
  expect_not_found "PATCH did not create object in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap "$target_b_name"

  run_cmd "seed delete-only resource in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" create configmap "$delete_a_name" --from-literal=value=a
  run_cmd "DELETE routes only where named object exists" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" delete configmap "$delete_a_name"
  expect_not_found "DELETE removed object from kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap "$delete_a_name"

  run_cmd "seed debug pod only in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" run "$debug_pod_name" --image=busybox:1.37 --restart=Never --command -- sh -c 'sleep 300'
  run_cmd "wait for debug pod readiness" kubectl_ctx "$CTX_B" -n "$NS" wait --for=condition=Ready "pod/$debug_pod_name" --timeout=90s
  expect_not_found "debug pod absent from kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get pod "$debug_pod_name"
  run_cmd "kubectl debug routes ephemeral container to owning cluster" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" debug "pod/$debug_pod_name" --image=busybox:1.37 --target="$debug_pod_name" --container="$debug_container" --quiet
  debug_name="$(kubectl_ctx "$CTX_B" -n "$NS" get pod "$debug_pod_name" -o jsonpath='{.spec.ephemeralContainers[?(@.name=="debugger")].name}' 2>&1)"
  if [[ "$debug_name" == "$debug_container" ]]; then
    add_result "PASS" "kubectl debug modifies only kubeconfig-proxy-b pod" "ephemeral container is present"
  else
    add_result "FAIL" "kubectl debug modifies only kubeconfig-proxy-b pod" "$debug_name"
  fi
  expect_not_found "kubectl debug did not create pod in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get pod "$debug_pod_name"

  readonly_output="$(kubectl_ctx "$RO_PROXY_CONTEXT" -n "$NS" create configmap "$readonly_name" --from-literal=value=blocked 2>&1)"
  if [[ "$readonly_output" == *"Forbidden"* || "$readonly_output" == *"read-only proxy rejects"* ]]; then
    add_result "PASS" "read-only proxy rejects mutation" "$readonly_output"
  else
    add_result "FAIL" "read-only proxy rejects mutation" "$readonly_output"
  fi
  expect_not_found "read-only did not create object in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" get configmap "$readonly_name"
  expect_not_found "read-only did not create object in kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get configmap "$readonly_name"
}
