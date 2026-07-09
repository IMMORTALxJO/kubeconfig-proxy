# Testing with kind

This guide shows how to test `kubeconfig-proxy` with two local Kubernetes
clusters created by [kind](https://kind.sigs.k8s.io/).

## Prerequisites

Install:

- Go 1.23+
- Docker
- kubectl
- kind

## Create local clusters

```bash
kind create cluster --name proxy-a
kind create cluster --name proxy-b
```

Check that both contexts exist:

```bash
kubectl config get-contexts kind-proxy-a kind-proxy-b
```

## Start kubeconfig-proxy

Run the proxy from the repository root:

```bash
go run ./cmd/kubeconfig-proxy \
  --contexts kind-proxy-a,kind-proxy-b \
  --primary-context kind-proxy-a \
  --output /tmp/kubeconfig-proxy.kind.yaml \
  --listen 127.0.0.1:9443 \
  --request-timeout 30s \
  --retries 1 \
  --retry-backoff 200ms
```

Keep this process running. In another terminal, point `kubectl` at the generated
proxy kubeconfig:

```bash
export KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml
kubectl cluster-info
```

`cluster-info` is a discovery-style command, so it is proxied only to
`kind-proxy-a`.

## Test list aggregation

Create different ConfigMaps directly in each original kind cluster:

```bash
kubectl --context kind-proxy-a create configmap only-a --from-literal=value=a
kubectl --context kind-proxy-b create configmap only-b --from-literal=value=b
```

List through the proxy kubeconfig:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml kubectl get configmaps
```

Expected result: both `only-a` and `only-b` are visible in the same output.

To see which source context each item came from:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml kubectl get configmaps -o yaml
```

Each aggregated item has this annotation:

```yaml
kubeconfig-proxy.io/context: kind-proxy-a
```

or:

```yaml
kubeconfig-proxy.io/context: kind-proxy-b
```

The proxy also injects a virtual `context` label into aggregated responses, so
you can show the source context directly in table output:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml kubectl get configmaps -L context
```

## Test fan-out mutations

Create a ConfigMap through the proxy:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml \
  kubectl create configmap fanout-demo --from-literal=value=shared
```

Check both original clusters:

```bash
kubectl --context kind-proxy-a get configmap fanout-demo
kubectl --context kind-proxy-b get configmap fanout-demo
```

Expected result: `fanout-demo` exists in both clusters.

## Test annotation-based routing

Create a ConfigMap that targets one specific source context:

```bash
cat <<'EOF' | KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: context-name-demo
  annotations:
    kubeconfig.proxy/context-name: kind-proxy-b
data:
  value: only-b
EOF
```

Check both original clusters:

```bash
kubectl --context kind-proxy-a get configmap context-name-demo
kubectl --context kind-proxy-b get configmap context-name-demo
```

Expected result: `context-name-demo` exists only in `kind-proxy-b`.

Create another ConfigMap that can be placed in any single context:

```bash
cat <<'EOF' | KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: single-context-demo
  annotations:
    kubeconfig.proxy/single-context: "true"
data:
  value: first-context
EOF
```

Check both original clusters:

```bash
kubectl --context kind-proxy-a get configmap single-context-demo
kubectl --context kind-proxy-b get configmap single-context-demo
```

Expected result: `single-context-demo` exists only in `kind-proxy-a`, because
`kind-proxy-a` is the first selected context by alphabetical context name.

## Cleanup

Stop the `kubeconfig-proxy` process, then remove the kind clusters:

```bash
kind delete cluster --name proxy-a
kind delete cluster --name proxy-b
rm -f /tmp/kubeconfig-proxy.kind.yaml
```
