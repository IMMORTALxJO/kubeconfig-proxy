# Kubernetes Compatibility

`kubeconfig-proxy` maintains a moving compatibility window for the Kubernetes
minor releases that are actively supported upstream. The window in this
repository is Kubernetes and kubectl `1.34`, `1.35`, and `1.36`.

The exact binaries and node images are pinned in `e2e/versions.sh`. The kind
project does not publish a node image for every Kubernetes patch release, so a
profile validates the API minor using the listed representative patch version.
The profile names, not untested patch releases, are the compatibility contract.
The current result is the latest run of the [Kubernetes compatibility
workflow](https://github.com/IMMORTALxJO/kubeconfig-proxy/actions/workflows/compatibility.yml).

| Profile | API server / kind node | kubectl | Kubernetes source tag |
| --- | --- | --- | --- |
| 1.34 | v1.34.0 | v1.34.0 | v1.34.0 |
| 1.35 | v1.35.0 | v1.35.0 | v1.35.0 |
| 1.36 | v1.36.1 | v1.36.1 | v1.36.1 |

## Compatibility Matrix

The `Kubernetes compatibility` workflow runs every kubectl and Kubernetes
profile pairing below. Both kind clusters use the same Kubernetes profile in
each cell while still exercising multi-context routing and aggregation.

| kubectl | Kubernetes / kind clusters | Cells |
| --- | --- | --- |
| 1.34 | 1.34, 1.35, 1.36 | 3 |
| 1.35 | 1.34, 1.35, 1.36 | 3 |
| 1.36 | 1.34, 1.35, 1.36 | 3 |

All 9 combinations run only when a maintainer starts the
`Kubernetes compatibility` workflow manually. It is not a required merge
check.

## Supported Scope

The matrix verifies the routing contract for Kubernetes stable APIs: discovery,
CRUD and server-side mutations, lists and pagination, watches, pod connection
subresources, read-only contexts, Helm release storage, and source markers.
It does not claim feature equivalence for alpha/beta APIs, arbitrary CRDs,
aggregated API implementations, kubectl plugins, or provider-specific auth
plugins.

Kubernetes supports kubectl within one minor version of a kube-apiserver. The
matrix also runs the two edge pairings with a two-minor skew as compatibility
probes; they do not expand the upstream-supported skew policy. `client-go`
`v0.36` remains the proxy's upstream client and is tested against all three
cluster profiles. See the upstream [version skew policy](https://kubernetes.io/releases/version-skew-policy/)
and [client-go compatibility matrix](https://github.com/kubernetes/client-go).

## Running a Cell Locally

Use the profile variables to run an individual matrix cell. Recreate local kind
clusters when changing either cluster profile:

```bash
KCP_RECREATE_KIND=1 \
KCP_KUBECTL_VERSION_PROFILE=1.34 \
KCP_CLUSTER_A_VERSION_PROFILE=1.35 \
KCP_CLUSTER_B_VERSION_PROFILE=1.35 \
KCP_SKIP_WERF=1 \
e2e/run.sh
```

The upstream kubectl client suite is not part of the compatibility workflow.
To run it separately for a profile:

```bash
KCP_KUBERNETES_VERSION_PROFILE=1.35 e2e/run-upstream-kubectl-e2e.sh
```

The weekly `Refresh Kubernetes compatibility profiles` workflow updates this
document, `e2e/versions.sh`, its focused test, and the workflow matrix. It
opens a pull request only when the three supported minor profiles change.
