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

The script runs `make check`, creates or reuses local kind clusters named `proxy-a` and `proxy-b`, builds a temporary kubeconfig, adds proxy contexts using `bin/kubeconfig-proxy`, and validates real Kubernetes API behavior through `kubectl`.

Report the final Markdown status table from the script to the user. If the script exits non-zero, keep the table first and briefly mention the failed checks.

## Checks

The runner covers the first integration ring for this project:

- `make check` and the resulting `bin/kubeconfig-proxy` binary.
- Required local tools: `kind` and `kubectl`.
- Two kind clusters: `proxy-a` and `proxy-b` with contexts `kind-proxy-a` and `kind-proxy-b`.
- Proxy context creation and exec-credential auto-start.
- Aggregated list responses with the virtual `context` label.
- Fan-out create mutations.
- `kubeconfig-proxy.io/context-name` single-target routing.
- `kubeconfig-proxy.io/single-context` routing to the alphabetically first target.
- Named GET routing to the source cluster that contains the object.
- `kubectl logs` routing to the cluster that contains the pod.
- `kubectl exec` routing to the cluster that contains the pod.
- PATCH routing based on the existing object when the patch body has no annotations.
- DELETE routing only to clusters where the named object exists.
- Read-only proxy context rejection of mutating requests with `403`.
- `--helm-release-proxy` primary-only release storage reads.
- The `examples/werf` converge/dismiss flow.

## Options

Use environment variables only when needed:

- `KCP_KEEP_KIND=1` leaves clusters and temporary files in place for debugging.
- `KCP_RECREATE_KIND=1` deletes existing `proxy-a`/`proxy-b` kind clusters before testing.
- `KCP_SKIP_MAKE_CHECK=1` skips `make check` and uses the existing `bin/kubeconfig-proxy`.
- `KCP_SKIP_WERF=1` skips the werf example when `werf` or image pulls are not available locally.
- `KCP_WERF_NAMESPACE=<name>` sets the werf example namespace; by default the runner uses a unique temporary namespace.
- `KCP_TEST_TIMEOUT=<duration>` sets kubectl request timeout, default `30s`.
