# Deploying through kubeconfig-proxy with werf

This example shows how to deploy a simple werf project through
`kubeconfig-proxy` into two local kind clusters:

- `kind-kubeconfig-proxy-a`
- `kind-kubeconfig-proxy-b`

The chart contains:

- an nginx `Deployment`;
- an nginx `Service`;
- a one-shot `Job`.

The nginx resources are deployed to both clusters. The Job has
`kubeconfig-proxy.io/single-context: "true"`, so `kubeconfig-proxy` creates it only
in the first selected context by alphabetical name.

## Prerequisites

Complete [../kind.md](../kind.md) first. From the repository root, update the
proxy context with Helm/werf release-history compatibility enabled:

```bash
./kubeconfig-proxy add-context kind-proxy \
  --contexts kind-kubeconfig-proxy-a,kind-kubeconfig-proxy-b \
  --primary-context kind-kubeconfig-proxy-a \
  --request-timeout 30s \
  --retries 1 \
  --retry-backoff 200ms \
  --helm-release-proxy
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

werf converge --env kind --kube-context kind-proxy
```

While experimenting with uncommitted local changes, add `--dev`:

```bash
werf converge --env kind --dev --kube-context kind-proxy
```

With `--helm-release-proxy`, the proxy reads Helm release history from the
primary context only, while resource mutations are still sent to both kind
clusters. This keeps werf from seeing duplicate release Secret objects on the
next converge.

By default werf deploys this project into the namespace:

```text
kubeconfig-proxy-werf-kind
```

## Validate nginx fan-out

The nginx Deployment should exist in both clusters:

```bash
kubectl --context kind-proxy \
  -n kubeconfig-proxy-werf-kind get deploy,svc -L kcp-context
```

Expected result: nginx resources are visible twice, once from
`kind-kubeconfig-proxy-a` and once from `kind-kubeconfig-proxy-b`.

You can also check the original clusters directly:

```bash
kubectl --context kind-kubeconfig-proxy-a \
  -n kubeconfig-proxy-werf-kind get deploy,svc

kubectl --context kind-kubeconfig-proxy-b \
  -n kubeconfig-proxy-werf-kind get deploy,svc
```

## Validate single-context Job

The Job should exist only in `kind-kubeconfig-proxy-a`, because
`kind-kubeconfig-proxy-a` is the first selected context by alphabetical name:

```bash
kubectl --context kind-proxy \
  -n kubeconfig-proxy-werf-kind get job -L kcp-context
```

Direct cluster checks:

```bash
kubectl --context kind-kubeconfig-proxy-a \
  -n kubeconfig-proxy-werf-kind get job kubeconfig-proxy-werf-smoke

kubectl --context kind-kubeconfig-proxy-b \
  -n kubeconfig-proxy-werf-kind get job kubeconfig-proxy-werf-smoke
```

Expected result:

- `kind-kubeconfig-proxy-a`: Job exists;
- `kind-kubeconfig-proxy-b`: Job is not found.

To force the Job into a specific context instead, replace the Job annotation in
[.helm/templates/job.yaml](.helm/templates/job.yaml):

```yaml
kubeconfig-proxy.io/target-context: kind-kubeconfig-proxy-b
```

## Cleanup

Run the dismiss through the proxy:

```bash
werf dismiss --env kind --with-namespace --kube-context kind-proxy
```

If you need a hard cleanup, remove the namespace from both original kind
clusters:

```bash
for ctx in kind-kubeconfig-proxy-a kind-kubeconfig-proxy-b; do
  kubectl --context "$ctx" \
    delete namespace kubeconfig-proxy-werf-kind --ignore-not-found
done
```
