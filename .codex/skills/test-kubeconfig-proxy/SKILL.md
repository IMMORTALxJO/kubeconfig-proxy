---
name: test-kubeconfig-proxy
description: Run kubeconfig-proxy validation and local integration tests. Use when the user asks for /test-kubeconfig-proxy, wants to verify this repository end to end, wants kind-based integration checks for kubeconfig-proxy, or asks whether proxy features still work against real Kubernetes clusters.
---

# Test Kubeconfig Proxy

## Workflow

Run the bundled script from the repository root:

```bash
e2e/run.sh
```

Equivalent Make targets are `make e2e` for all suites, `make e2e kind` for the
two-cluster runner, `make e2e kubectl` for upstream compatibility, and
`make e2e local` for the built-in kind checks without `make check` or werf.

The script runs `make check`, rebuilds `bin/kubeconfig-proxy` with Go coverage
instrumentation, creates or reuses local kind clusters named
`kubeconfig-proxy-a` and `kubeconfig-proxy-b`, builds a temporary kubeconfig,
adds proxy contexts with serve logging enabled, and validates real Kubernetes
API behavior through pinned `kubectl`. At the end it stops detached proxy
processes, merges their
`GOCOVERDIR` data with coverage from CLI invocations, and prints the standard
per-function Go coverage report. It also writes the HTML report to
`.codex/reports/coverage.html`.

The runner pins Kubernetes and `kubectl` to `v1.36.1`. New kind clusters are
created with `kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5`.
If a reused kind cluster runs another Kubernetes version, the runner fails before
touching test resources; rerun with `KCP_RECREATE_KIND=1` to recreate it with the
pinned node image. The runner also verifies that kind nodes are Ready before
running proxy behavior checks. If the local `kubectl` is missing or not `v1.36.1`,
the runner uses a cached official `v1.36.1` binary or downloads one and verifies
its sha256 checksum. The default cache root is
`${XDG_CACHE_HOME:-$HOME/.cache}/kubeconfig-proxy`.

## Upstream kubectl compatibility

Run the upstream Kubernetes `[sig-cli] Kubectl client` e2e suite through a
single-source proxy context with:

```bash
e2e/run-upstream-kubectl-e2e.sh
```

The runner builds Kubernetes `v1.36.1` from a cached shallow checkout, creates
or reuses the `kubeconfig-proxy-kubectl-e2e` kind cluster, and runs `e2e.test`
with the direct source context for framework setup and assertions. A temporary
wrapper replaces the e2e framework's `--kubeconfig` and `--context` arguments
only for the upstream `kubectl` subprocess, forcing every tested CLI command
through `kind-proxy-kubectl-e2e`. It also removes only the source-cluster
`--server` argument injected by the framework; a distinct `--server` explicitly
used by a test is preserved. It deliberately does not pass `--host`, because
that would override the proxy server configured in the kubeconfig.
Before any e2e command it builds one coverage-instrumented
`bin/kubeconfig-proxy`; every CLI invocation and exec-credential-started proxy
process uses that file with serve logging enabled. The runner always writes a
final per-function and total coverage report from its `GOCOVERDIR` data and
creates `.codex/reports/coverage.html`.
The runner streams upstream Ginkgo output to the terminal in real time while
also retaining its per-step log file for failures.
The wrapper implements the upstream `kubectl.sh path` contract so the
in-cluster-config test can copy the real kubectl binary into its pod.
That test is excluded by default because it validates a direct in-pod
`kubernetes.default.svc` configuration rather than the host proxy kubeconfig;
set `KCP_KUBECTL_E2E_SKIP=` to include it deliberately.

This is a single-source transparency test, not a multi-cluster routing test:
the upstream e2e framework expects one coherent API server. Keep the regular
two-cluster runner as the integration coverage for aggregation and fan-out.

Useful options:

- `KCP_KUBECTL_E2E_FOCUS=<regexp>` overrides the default
  `[sig-cli] Kubectl client` Ginkgo focus for a fast targeted run.
- `KCP_KUBECTL_E2E_SKIP=<regexp>` skips selected Ginkgo specs.
- `KCP_KUBECTL_E2E_TIMEOUT=<duration>` changes the Ginkgo suite timeout,
  default `6h`.
- `KCP_KUBERNETES_SOURCE=<path>` uses an existing Kubernetes `v1.36.1`
  checkout instead of the cache.
- `KCP_KUBECTL_E2E_RECREATE_KIND=1` recreates the dedicated kind cluster.
- `KCP_KEEP_KIND=1` leaves the cluster, temporary kubeconfig, e2e report,
  proxy state, and enabled serve log in place for debugging.
- `KCP_COVERAGE_HTML=<path>` changes the HTML coverage report path; by default
  each e2e runner writes `.codex/reports/coverage.html` (the latest run wins).

Kubernetes `v1.36.1` requires Bash 4.2 or newer to build. On macOS, install it
with `brew install bash`; the runner finds the Homebrew path automatically, or
accepts `KCP_KUBECTL_E2E_BASH=/path/to/bash`. When no suitable Bash is
available, the runner builds native-platform binaries in Docker with
`golang:1.26.0-bookworm`; override that image with
`KCP_KUBECTL_E2E_BUILDER_IMAGE` if needed.
Set `KCP_KUBECTL_E2E_DOCKER_PULL_TIMEOUT_SECONDS` to change the Docker image
pull timeout (default `300`).

Report the final Markdown status table from the script to the user. If the script exits non-zero, keep the table first and briefly mention the failed checks.

## Checks

The runner covers the first integration ring for this project:

- `make check`.
- A fresh `bin/kubeconfig-proxy` built with `-cover`, `-covermode=atomic`, and
  `-coverpkg=./...`.
- Integration coverage collected from CLI and detached proxy processes and
  printed as a per-function report with a total statement percentage.
- Required local tools: `kind` and `curl` (the latter is used by the
  port-forward check and when the pinned `kubectl` client must be downloaded).
- Pinned `kubectl v1.36.1`.
- Two kind clusters: `kubeconfig-proxy-a` and `kubeconfig-proxy-b` running
  Kubernetes `v1.36.1` with Ready nodes, using contexts
  `kind-kubeconfig-proxy-a` and `kind-kubeconfig-proxy-b`.
- Proxy context creation, explicit coverage-instrumented serve process, and
  exec-credential access.
- Aggregated list responses with the virtual `context` label.
- Aggregated list pagination with a global limit and cross-context continuation.
- Duplicate source-context rejection before proxy state is written.
- Collision-resistant default state paths for context names that sanitize alike.
- Fan-out create mutations.
- `kubeconfig-proxy.io/context-name` single-target routing.
- `kubeconfig-proxy.io/single-context` routing to the alphabetically first target.
- Named GET routing to the source cluster that contains the object.
- `kubectl logs` routing to the cluster that contains the pod.
- `kubectl exec` routing to the cluster that contains the pod.
- `kubectl attach` and `kubectl port-forward` routing to the cluster that
  contains the pod.
- `kubectl debug` routing its ephemeral-container mutation to the cluster that
  contains the pod.
- `kubectl rollout restart` and `kubectl rollout status` for matching
  deployments in both source clusters.
- Multi-cluster `kubectl get -w` events from both source clusters.
- PATCH routing based on the existing object when the patch body has no annotations.
- DELETE routing only to clusters where the named object exists.
- Read-only proxy context allowing read requests and rejecting mutating requests with `403`.
- `--helm-release-proxy` primary-only release storage reads.
- The `examples/werf` converge/dismiss flow.

## Options

Use environment variables only when needed:

- `KCP_KEEP_KIND=1` leaves clusters and temporary files in place for debugging.
- `KCP_RECREATE_KIND=1` deletes existing `kubeconfig-proxy-a`/`kubeconfig-proxy-b` kind clusters before testing.
- `KCP_SKIP_MAKE_CHECK=1` skips `make check`; the coverage-instrumented binary
  is still rebuilt.
- `KCP_SKIP_WERF=1` skips the werf example when `werf` or image pulls are not available locally.
- `KCP_WERF_NAMESPACE=<name>` sets the werf example namespace; by default the runner uses a unique temporary namespace.
- `KCP_TEST_TIMEOUT=<duration>` sets kubectl request timeout, default `30s`.
- `KCP_CLUSTER_READY_TIMEOUT=<duration>` sets kind node readiness timeout, default `120s`.
- `KCP_WERF_TIMEOUT=<seconds>` sets werf resource tracking timeout, default `180`.
- `KCP_CACHE_DIR=<path>` overrides the cache root for downloaded pinned tools.

## Check layout

The two-cluster runner sources category files from `e2e/checks/`:

- `context.sh` validates proxy-context setup and state paths.
- `aggregation.sh` covers aggregate reads and pagination.
- `routing.sh` covers mutations, routing annotations, read-only mode, and
  `kubectl debug`.
- `rollout.sh` covers Deployment restart and status routing.
- `subresources.sh` covers logs, exec, attach, and port-forward.
- `watch.sh` covers multi-cluster watch events.
- `werf.sh` covers Helm storage and the werf example.

Keep checks as functions that use the runner's helpers (`run_cmd`,
`kubectl_ctx`, `expect_exists`, and `expect_not_found`) so failures remain in
the final result table and cleanup is centralized.
