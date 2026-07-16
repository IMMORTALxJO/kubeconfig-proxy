# kubeconfig-proxy

`kubeconfig-proxy` is a local Kubernetes API proxy that adds an auto-started
proxy context to your kubeconfig. That context can work with several source
kubeconfig contexts at the same time.

It is useful when you want to run ordinary Kubernetes tools against a group of
clusters as if they were one logical target:

- inspect resources from multiple clusters in one `kubectl get`;
- create or update the same resource in every selected cluster;
- route selected resources to one specific cluster with annotations;
- deploy simple Helm/werf projects through one proxy context.

The proxy context is backed by a local state file and a Kubernetes exec
credential plugin. The local proxy uses HTTPS and bearer-token authentication.

## How It Works

The proxy keeps a list of source contexts from the original kubeconfig. Requests
made through the proxy context are routed according to request type:

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

Add an auto-started proxy context to your current kubeconfig:

```bash
kubeconfig-proxy add-context prod-proxy \
  --kubeconfig ~/.kube/config \
  --contexts prod-a,prod-b \
  --primary-context prod-a \
  --proxy-ttl 10m
```

Use it like any other kubeconfig context:

```bash
kubectl --context prod-proxy get pods -A -L context
```

The generated `prod-proxy` context points to a local HTTPS endpoint and uses a
kubeconfig exec credential command. When `kubectl` uses that context, it runs
`kubeconfig-proxy credential --state <path>`. The credential command starts
`kubeconfig-proxy serve --state <path>` automatically if the proxy is not
already running, waits for readiness, and returns the bearer token expected by
the local proxy.

The state file defaults to:

```text
~/.kube/kubeconfig-proxy/<context-name>.yaml
```

It is written with file mode `0600` and contains the proxy's private TLS key,
certificate, bearer token, selected source contexts, primary context, listen
address, and runtime options. The kubeconfig stores only the public proxy server
URL, certificate authority data, and exec command.

`--proxy-ttl` controls idle shutdown. If no proxied Kubernetes API requests are
active for that duration, the auto-started proxy process exits by itself. Health
checks made by the credential command do not extend the TTL. Set `--proxy-ttl 0`
to disable idle shutdown.

You can also select source contexts with a regular expression:

```bash
kubeconfig-proxy add-context prod-proxy \
  --kubeconfig ~/.kube/config \
  --context-regexp '^prod-' \
  --primary-context prod-a
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
kubeconfig-proxy add-context dev-stage-proxy \
  --kubeconfig ~/.kube/config \
  --contexts dev,stage \
  --primary-context dev \
  --helm-release-proxy
```

With this mode enabled, Helm release storage list/watch requests are read only
from the primary context, while ordinary application resources are still fanned
out to all selected contexts.

See [examples/werf/README.md](examples/werf/README.md) for a complete local
werf example.

## Commands And Flags

- `add-context <name>` adds an auto-started proxy context to a kubeconfig.
- `--kubeconfig ~/.kube/config` selects the source kubeconfig. If omitted,
  standard Kubernetes kubeconfig loading rules are used.
- `--contexts dev,stage,prod` limits the proxy to specific source contexts.
- `--context-regexp '^prod-'` selects source contexts by regular expression.
- `--primary-context dev` selects the context used for discovery and other
  primary-only operations.
- `--listen 127.0.0.1:9443` sets the proxy listen address.
- `--proxy-ttl 10m` sets the auto-started proxy idle lifetime. Use `0` to
  disable idle shutdown.
- `--request-timeout 30s` sets the timeout for one upstream Kubernetes API
  request. Use `0` to disable it.
- `--retries 5` retries temporary upstream failures.
- `--retry-backoff 500ms` sets the delay between retry attempts.
- `--helm-release-proxy` enables Helm/werf release-history compatibility mode.
- `credential --state <path>` is the kubeconfig exec credential entrypoint.
- `serve --state <path>` runs a state-backed proxy process.

Retries default to `5`. Set `--retries 0` to disable them. The proxy retries
network errors and temporary upstream HTTP responses: `429`, `500`, `502`,
`503`, and `504`.

## Security

The proxy uses the credentials from the source kubeconfig to talk to the source
clusters. Protect the listen address accordingly.

When a context is added, the proxy generates:

- a bearer token required by every incoming request;
- a self-signed TLS certificate and private key for the local proxy.

The state file is written with file mode `0600` and contains the bearer token
and TLS private key. Keep the proxy bound to `127.0.0.1` unless you
intentionally want to expose it to a trusted network.

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

## License

Apache License 2.0. See [LICENSE](LICENSE).
