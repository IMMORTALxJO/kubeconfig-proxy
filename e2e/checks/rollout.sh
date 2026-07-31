#!/usr/bin/env bash

# Sourced by e2e/run.sh after both proxy contexts have started.

# Invoked indirectly through run_cmd.
# shellcheck disable=SC2329
apply_delayed_rollout_deployment() {
  local ctx="$1"
  local deployment_name="$2"

  kubectl_ctx "$ctx" -n "$NS" apply -f - <<EOF_DEPLOYMENT
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $deployment_name
spec:
  replicas: 1
  selector:
    matchLabels:
      app: $deployment_name
  template:
    metadata:
      labels:
        app: $deployment_name
    spec:
      containers:
        - name: rollout
          image: busybox:1.37
          command: ["sh", "-c", "rm -f /tmp/ready; sleep 8; touch /tmp/ready; sleep 300"]
          readinessProbe:
            exec:
              command: ["sh", "-c", "test -f /tmp/ready"]
            periodSeconds: 1
EOF_DEPLOYMENT
}

run_rollout_checks() {
  local first_restarted_at
  local restarted_a_at
  local restarted_b_at
  local deployment_name

  deployment_name="$(e2e_resource_name rollout)"

  run_cmd "seed rollout deployment in kubeconfig-proxy-a" kubectl_ctx "$CTX_A" -n "$NS" create deployment "$deployment_name" --image=busybox:1.37 -- sh -c 'sleep 300'
  expect_not_found "rollout deployment absent from kubeconfig-proxy-b" kubectl_ctx "$CTX_B" -n "$NS" get deployment "$deployment_name"

  run_cmd "kubectl rollout restart routes to owning cluster" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" rollout restart "deployment/$deployment_name"
  first_restarted_at="$(kubectl_ctx "$CTX_A" -n "$NS" get deployment "$deployment_name" -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}' 2>&1)"
  if [[ -n "$first_restarted_at" ]]; then
    add_result "PASS" "rollout restart changed only kubeconfig-proxy-a deployment" "restart annotation: $first_restarted_at"
  else
    add_result "FAIL" "rollout restart changed only kubeconfig-proxy-a deployment" "$first_restarted_at"
  fi

  run_cmd "kubectl rollout status reaches owning cluster" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" rollout status "deployment/$deployment_name" --timeout=90s

  run_cmd "seed delayed matching rollout deployment in kubeconfig-proxy-b" apply_delayed_rollout_deployment "$CTX_B" "$deployment_name"
  sleep 1
  run_cmd "kubectl rollout restart restarts deployments in both clusters" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" rollout restart "deployment/$deployment_name"
  restarted_a_at="$(kubectl_ctx "$CTX_A" -n "$NS" get deployment "$deployment_name" -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}' 2>&1)"
  restarted_b_at="$(kubectl_ctx "$CTX_B" -n "$NS" get deployment "$deployment_name" -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}' 2>&1)"
  if [[ "$restarted_a_at" != "$first_restarted_at" && -n "$restarted_b_at" ]]; then
    add_result "PASS" "rollout restart changed deployments in both source clusters" "restart annotation: $restarted_a_at"
  else
    add_result "FAIL" "rollout restart changed deployments in both source clusters" "kubeconfig-proxy-a: $restarted_a_at; kubeconfig-proxy-b: $restarted_b_at"
  fi

  run_cmd "kubectl rollout status supports matching deployments" kubectl_ctx "$PROXY_CONTEXT" -n "$NS" rollout status "deployment/$deployment_name" --timeout=90s
}
