# kubeconfig-proxy

`kubeconfig-proxy` starts a local Kubernetes API proxy for every context in a
source kubeconfig and writes a new kubeconfig that points `kubectl` to that
proxy.

On every start, the proxy generates a temporary bearer token, writes it into the
generated kubeconfig, and requires it on every incoming request.

Current MVP behavior:

- list requests are aggregated from all selected source contexts;
- watch requests are streamed from all selected source contexts;
- create, update, patch and delete requests are sent to all selected contexts;
- resources can opt into single-context mutations with annotations;
- logs, exec, attach, port-forward and discovery requests are proxied only to
  one context.

When `--helm-release-proxy` is enabled, Helm release storage list/watch requests
are proxied only to the primary context. Helm and werf expect release history to
be a single linear stream; returning the same release Secret or ConfigMap from
multiple clusters can break their release planner.

## Usage

```bash
GOTOOLCHAIN=auto go run ./cmd/kubeconfig-proxy \
  --kubeconfig ~/.kube/config \
  --output ~/.kube/config.proxy
```

In another terminal:

```bash
KUBECONFIG=~/.kube/config.proxy kubectl get pods -A
```

Useful flags:

- `--contexts dev,stage,prod` limits the proxy to specific source contexts;
- `--primary-context dev` selects the target for primary-only API calls;
- `--listen 127.0.0.1:9443` uses a stable local address;
- `--request-timeout 30s` sets the timeout for one upstream Kubernetes API
  request; use `0` to disable it;
- `--retries 2` retries failed upstream requests twice;
- `--retry-backoff 500ms` sets the delay between retry attempts;
- `--helm-release-proxy` reads Helm release history only from the primary
  context for Helm/werf compatibility.

Retries are disabled by default. When enabled, the proxy retries network errors
and temporary upstream HTTP responses: `429`, `500`, `502`, `503` and `504`.

## Resource routing annotations

By default, mutating requests are sent to every selected context.

Add this annotation to create or update a resource only in one specific source
context:

```yaml
metadata:
  annotations:
    kubeconfig-proxy.io/context-name: dev
```

Add this annotation to create or update a resource only in one context selected
by the proxy. The selected context is the first one by alphabetical context name:

```yaml
metadata:
  annotations:
    kubeconfig-proxy.io/single-context: "true"
```

If both annotations are present, `kubeconfig-proxy.io/context-name` wins.

The proxy annotates aggregated list items with:

```yaml
kubeconfig-proxy.io/context: <source-context>
```

It also injects a virtual `context` label into aggregated responses, so the
source context can be shown in table output:

```bash
kubectl get pods -A -L context
```

Named pod subresources such as `kubectl logs <pod>` are resolved against the
cluster that contains the pod.
Upgrade-based subresources such as `exec`, `attach` and `port-forward` are
proxied as upgraded bidirectional streams.
