# kubeconfig-proxy

`kubeconfig-proxy` is a local Kubernetes API proxy that lets one generated
kubeconfig work with several source kubeconfig contexts at the same time.

It is useful when you want to run ordinary Kubernetes tools against a group of
clusters as if they were one logical target:

- inspect resources from multiple clusters in one `kubectl get`;
- create or update the same resource in every selected cluster;
- route selected resources to one specific cluster with annotations;
- deploy simple Helm/werf projects through one proxy kubeconfig.

On startup, the proxy reads a source kubeconfig, starts a local HTTPS proxy, and
writes a new kubeconfig that points to that proxy. The generated kubeconfig
contains temporary proxy credentials, so only clients with that file can use the
local proxy.

## How It Works

The proxy keeps a list of source contexts from the original kubeconfig. Requests
made through the generated kubeconfig are routed according to request type:

- list requests are aggregated from all selected contexts;
- watch requests are streamed from all selected contexts;
- create, update, patch, and delete requests are sent to all selected contexts;
- discovery requests use the primary context;
- named pod subresources such as `logs`, `exec`, `attach`, and `port-forward`
  are routed to the context that contains the pod;
- resources can opt into single-context mutation with annotations.

Aggregated objects are marked with:

```yaml
kubeconfig-proxy.io/context: <source-context>
```

The proxy also injects a virtual `context` label into aggregated responses, so
you can display the source cluster directly:

```bash
kubectl get pods -A -L context
```

## Quick Start

Run the compiled binary:

```bash
kubeconfig-proxy \
  --kubeconfig ~/.kube/config \
  --contexts dev,stage \
  --primary-context dev \
  --output ~/.kube/config.proxy \
  --listen 127.0.0.1:9443
```

Keep the process running. In another terminal, use the generated kubeconfig:

```bash
KUBECONFIG=~/.kube/config.proxy kubectl get namespaces -L context
```

If `--contexts` is omitted, all contexts from the source kubeconfig are used. If
`--primary-context` is omitted, the source kubeconfig current context is used;
when there is no current context, the first selected context is used.

## Resource Routing Annotations

By default, mutating requests are sent to every selected context.

To create or update a resource only in one specific source context, add:

```yaml
metadata:
  annotations:
    kubeconfig-proxy.io/context-name: dev
```

To create or update a resource in any single context, add:

```yaml
metadata:
  annotations:
    kubeconfig-proxy.io/single-context: "true"
```

The selected context for `single-context` is the first configured context by
alphabetical name. If both annotations are present,
`kubeconfig-proxy.io/context-name` wins.

## Helm And Werf

Helm and werf store release history in Kubernetes Secrets or ConfigMaps and
expect that history to be a single linear stream. If the proxy returns the same
release record from several clusters, their release planner can fail.

Use `--helm-release-proxy` when deploying Helm/werf projects through the proxy:

```bash
kubeconfig-proxy \
  --kubeconfig ~/.kube/config \
  --contexts dev,stage \
  --primary-context dev \
  --output ~/.kube/config.proxy \
  --listen 127.0.0.1:9443 \
  --helm-release-proxy
```

With this mode enabled, Helm release storage list/watch requests are read only
from the primary context, while ordinary application resources are still fanned
out to all selected contexts.

See [examples/werf/README.md](examples/werf/README.md) for a complete local
werf example.

## Flags

- `--kubeconfig ~/.kube/config` selects the source kubeconfig. If omitted,
  standard Kubernetes kubeconfig loading rules are used.
- `--output ~/.kube/config.proxy` sets the generated proxy kubeconfig path.
- `--contexts dev,stage,prod` limits the proxy to specific source contexts.
- `--primary-context dev` selects the context used for discovery and other
  primary-only operations.
- `--listen 127.0.0.1:9443` sets the proxy listen address.
- `--request-timeout 30s` sets the timeout for one upstream Kubernetes API
  request. Use `0` to disable it.
- `--retries 2` retries temporary upstream failures.
- `--retry-backoff 500ms` sets the delay between retry attempts.
- `--helm-release-proxy` enables Helm/werf release-history compatibility mode.

Retries are disabled by default. When enabled, the proxy retries network errors
and temporary upstream HTTP responses: `429`, `500`, `502`, `503`, and `504`.

## Security

The proxy uses the credentials from the source kubeconfig to talk to the source
clusters. Protect the listen address accordingly.

On every startup, the proxy generates:

- a temporary bearer token required by every incoming request;
- a temporary self-signed TLS certificate for the generated kubeconfig.

The generated kubeconfig is written with file mode `0600`. Keep the proxy bound
to `127.0.0.1` unless you intentionally want to expose it to a trusted network.

## Local Examples

- [examples/kind.md](examples/kind.md) shows how to test the proxy with two
  local kind clusters.
- [examples/werf/README.md](examples/werf/README.md) shows how to deploy a
  simple nginx chart and a single-context Job through werf.

## Development

Run tests:

```bash
GOTOOLCHAIN=auto go test ./...
GOTOOLCHAIN=auto go test -race ./internal/proxy
```

Build the binary:

```bash
GOTOOLCHAIN=auto go build -trimpath -o kubeconfig-proxy ./cmd/kubeconfig-proxy
```

Release builds are produced by GitHub Actions when a `v*` tag is pushed.
