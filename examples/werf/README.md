# Deploying through kubeconfig-proxy with werf

This example shows how to deploy a simple werf project through
`kubeconfig-proxy` into two local kind clusters:

- `kind-proxy-a`
- `kind-proxy-b`

The chart contains:

- an nginx `Deployment`;
- an nginx `Service`;
- a one-shot `Job`.

The nginx resources are deployed to both clusters. The Job has
`kubeconfig.proxy/single-context: "true"`, so `kubeconfig-proxy` creates it only
in the first selected context by alphabetical name.

## Prerequisites

Complete [../kind.md](../kind.md) first and keep `kubeconfig-proxy` running:

```bash
GOTOOLCHAIN=auto go run ./cmd/kubeconfig-proxy \
  --kubeconfig /Users/aleksandr.prusov/.kube/proxy-config \
  --contexts kind-proxy-a,kind-proxy-b \
  --primary-context kind-proxy-a \
  --output /tmp/kubeconfig-proxy.kind.yaml \
  --listen 127.0.0.1:9443 \
  --request-timeout 30s \
  --retries 1 \
  --retry-backoff 200ms
```

Install werf if it is not installed yet:

```bash
curl -sSL https://werf.io/install.sh | bash
```

werf deploys local Helm-style charts with `werf converge`; the official docs
show the same minimal shape: `.helm/templates/*` plus `werf.yaml`.

## Deploy

Run from this directory:

```bash
cd examples/werf

KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml \
  werf converge --env kind
```

While experimenting with uncommitted local changes, add `--dev`:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml \
  werf converge --env kind --dev
```

The proxy reads Helm release history from the primary context only, while
resource mutations are still sent to both kind clusters. This keeps werf from
seeing duplicate release Secret objects on the next converge.

By default werf deploys this project into the namespace:

```text
kubeconfig-proxy-werf-kind
```

## Validate nginx fan-out

The nginx Deployment should exist in both clusters:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml \
  kubectl -n kubeconfig-proxy-werf-kind get deploy,svc -L context
```

Expected result: nginx resources are visible twice, once from `kind-proxy-a`
and once from `kind-proxy-b`.

You can also check the original clusters directly:

```bash
kubectl --kubeconfig /Users/aleksandr.prusov/.kube/proxy-config \
  --context kind-proxy-a \
  -n kubeconfig-proxy-werf-kind get deploy,svc

kubectl --kubeconfig /Users/aleksandr.prusov/.kube/proxy-config \
  --context kind-proxy-b \
  -n kubeconfig-proxy-werf-kind get deploy,svc
```

## Validate single-context Job

The Job should exist only in `kind-proxy-a`, because `kind-proxy-a` is the first
selected context by alphabetical name:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml \
  kubectl -n kubeconfig-proxy-werf-kind get job -L context
```

Direct cluster checks:

```bash
kubectl --kubeconfig /Users/aleksandr.prusov/.kube/proxy-config \
  --context kind-proxy-a \
  -n kubeconfig-proxy-werf-kind get job kubeconfig-proxy-werf-smoke

kubectl --kubeconfig /Users/aleksandr.prusov/.kube/proxy-config \
  --context kind-proxy-b \
  -n kubeconfig-proxy-werf-kind get job kubeconfig-proxy-werf-smoke
```

Expected result:

- `kind-proxy-a`: Job exists;
- `kind-proxy-b`: Job is not found.

To force the Job into a specific context instead, replace the Job annotation in
[.helm/templates/job.yaml](.helm/templates/job.yaml):

```yaml
kubeconfig.proxy/context-name: kind-proxy-b
```

## Cleanup

Run the dismiss through the proxy:

```bash
KUBECONFIG=/tmp/kubeconfig-proxy.kind.yaml \
  werf dismiss --env kind --with-namespace
```

If you need a hard cleanup, remove the namespace from both original kind
clusters:

```bash
for ctx in kind-proxy-a kind-proxy-b; do
  kubectl --kubeconfig /Users/aleksandr.prusov/.kube/proxy-config \
    --context "$ctx" \
    delete namespace kubeconfig-proxy-werf-kind --ignore-not-found
done
```
