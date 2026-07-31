# Testing with kind

This guide shows how to test `kubeconfig-proxy` with two local Kubernetes
clusters created by [kind](https://kind.sigs.k8s.io/).

## Prerequisites

Install:

- Go 1.26.5+
- Docker
- kubectl
- kind

If your local `go` binary is older but supports toolchain downloads, run the Go
commands below with `GOTOOLCHAIN=auto`. For example:

```bash
GOTOOLCHAIN=auto go test ./...
```

## Create local clusters

```bash
kind create cluster --name kubeconfig-proxy-a
kind create cluster --name kubeconfig-proxy-b
```

Check that both contexts exist:

```bash
kubectl config get-contexts kind-kubeconfig-proxy-a kind-kubeconfig-proxy-b
```

## Add the proxy context

Build the binary from the repository root:

```bash
GOTOOLCHAIN=auto go build -trimpath -o ./kubeconfig-proxy ./cmd/kubeconfig-proxy
```

Add an auto-started proxy context to your kubeconfig:

```bash
./kubeconfig-proxy add-context kind-proxy \
  --contexts kind-kubeconfig-proxy-a,kind-kubeconfig-proxy-b \
  --primary-context kind-kubeconfig-proxy-a \
  --request-timeout 30s \
  --retries 1 \
  --retry-backoff 200ms
```

Use the proxy context like any other kubeconfig context:

```bash
kubectl --context kind-proxy cluster-info
```

`cluster-info` is a discovery-style command, so it is proxied only to
`kind-kubeconfig-proxy-a`.

## Test list aggregation

Create different ConfigMaps directly in each original kind cluster:

```bash
kubectl --context kind-kubeconfig-proxy-a create configmap only-a --from-literal=value=a
kubectl --context kind-kubeconfig-proxy-b create configmap only-b --from-literal=value=b
```

List through the proxy context:

```bash
kubectl --context kind-proxy get configmaps
```

Expected result: both `only-a` and `only-b` are visible in the same output.

To see which source context each item came from:

```bash
kubectl --context kind-proxy get configmaps -o yaml
```

Each aggregated item has this annotation:

```yaml
kubeconfig-proxy.io/source-context: kind-kubeconfig-proxy-a
```

or:

```yaml
kubeconfig-proxy.io/source-context: kind-kubeconfig-proxy-b
```

The proxy also injects a virtual `kcp-context` label into aggregated responses, so
you can show the source context directly in table output:

```bash
kubectl --context kind-proxy get configmaps -L kcp-context
```

## Test fan-out mutations

Create a ConfigMap through the proxy:

```bash
kubectl --context kind-proxy create configmap fanout-demo --from-literal=value=shared
```

Check both original clusters:

```bash
kubectl --context kind-kubeconfig-proxy-a get configmap fanout-demo
kubectl --context kind-kubeconfig-proxy-b get configmap fanout-demo
```

Expected result: `fanout-demo` exists in both clusters.

## Test annotation-based routing

Create a ConfigMap that targets one specific source context:

```bash
cat <<'EOF' | kubectl --context kind-proxy apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: context-name-demo
  annotations:
    kubeconfig-proxy.io/target-context: kind-kubeconfig-proxy-b
data:
  value: only-b
EOF
```

Check both original clusters:

```bash
kubectl --context kind-kubeconfig-proxy-a get configmap context-name-demo
kubectl --context kind-kubeconfig-proxy-b get configmap context-name-demo
```

Expected result: `context-name-demo` exists only in `kind-kubeconfig-proxy-b`.

Create another ConfigMap that can be placed in any single context:

```bash
cat <<'EOF' | kubectl --context kind-proxy apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: single-context-demo
  annotations:
    kubeconfig-proxy.io/single-context: "true"
data:
  value: first-context
EOF
```

Check both original clusters:

```bash
kubectl --context kind-kubeconfig-proxy-a get configmap single-context-demo
kubectl --context kind-kubeconfig-proxy-b get configmap single-context-demo
```

Expected result: `single-context-demo` exists only in `kind-kubeconfig-proxy-a`, because
`kind-kubeconfig-proxy-a` is the first selected context by alphabetical context name.

## Cleanup

Remove the kind clusters:

```bash
kind delete cluster --name kubeconfig-proxy-a
kind delete cluster --name kubeconfig-proxy-b
```
