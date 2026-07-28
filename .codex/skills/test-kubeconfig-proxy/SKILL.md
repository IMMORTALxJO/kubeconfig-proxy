---
name: test-kubeconfig-proxy
description: Run kubeconfig-proxy validation and local integration tests. Use when the user asks for /test-kubeconfig-proxy, wants to verify this repository end to end, wants kind-based integration checks for kubeconfig-proxy, or asks whether proxy features still work against real Kubernetes clusters.
---

# Test Kubeconfig Proxy

## Workflow

Run the bundled script from the repository root:

```bash
.codex/skills/test-kubeconfig-proxy/scripts/run.sh
```

The script runs `make check`, rebuilds `bin/kubeconfig-proxy` with Go coverage
instrumentation, creates or reuses local kind clusters named
`kubeconfig-proxy-a` and `kubeconfig-proxy-b`, builds a temporary kubeconfig,
adds proxy contexts, and validates real Kubernetes API behavior through pinned
`kubectl`. At the end it stops detached proxy processes, merges their
`GOCOVERDIR` data with coverage from CLI invocations, and prints the standard
per-function Go coverage report.

The runner pins Kubernetes and `kubectl` to `v1.36.1`. New kind clusters are
created with `kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5`.
If a reused kind cluster runs another Kubernetes version, the runner fails before
touching test resources; rerun with `KCP_RECREATE_KIND=1` to recreate it with the
pinned node image. The runner also verifies that kind nodes are Ready before
running proxy behavior checks. If the local `kubectl` is missing or not `v1.36.1`,
the runner uses a cached official `v1.36.1` binary or downloads one and verifies
its sha256 checksum. The default cache root is
`${XDG_CACHE_HOME:-$HOME/.cache}/kubeconfig-proxy`.

Report the final Markdown status table from the script to the user. If the script exits non-zero, keep the table first and briefly mention the failed checks.

## Checks

The runner covers the first integration ring for this project:

- `make check`.
- A fresh `bin/kubeconfig-proxy` built with `-cover`, `-covermode=atomic`, and
  `-coverpkg=./...`.
- Integration coverage collected from CLI and detached proxy processes and
  printed as a per-function report with a total statement percentage.
- Required local tool: `kind`; `curl` is required only when the pinned `kubectl`
  client must be downloaded.
- Pinned `kubectl v1.36.1`.
- Two kind clusters: `kubeconfig-proxy-a` and `kubeconfig-proxy-b` running
  Kubernetes `v1.36.1` with Ready nodes, using contexts
  `kind-kubeconfig-proxy-a` and `kind-kubeconfig-proxy-b`.
- Proxy context creation and exec-credential auto-start.
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
- Named `GET` and `PATCH` scale subresources routing to the cluster that
  contains the deployment.
- Named `POST` eviction subresources routing to the cluster that contains the
  pod.
- Named-field-selector watches remaining open until a future matching object
  appears.
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
