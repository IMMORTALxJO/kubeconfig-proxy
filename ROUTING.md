# Routing Contract

This document is the normative routing matrix for `kubeconfig-proxy`.  It
defines the intended behaviour of the proxy as an HTTP API in front of several
Kubernetes contexts.  `kubectl` compatibility is the first constraint: a
request must have an explicit class, target-selection rule, and response rule
before it is implemented.

The captured corpus in
`.codex/skills/gen-kubectl-commands/kubectl-commands.yaml` is the evidence for
the initial matrix.  It currently covers 120 `kubectl` command variants and
137 observed HTTP exchanges.  The rules below are path-pattern rules, not a
closed list of the resources seen in that corpus, so they also apply to custom
resources and API groups not yet present in it.

This is a target contract.  Documentation alone does not change proxy
behaviour; every row that changes the implementation must be delivered with
unit tests that assert the exact upstream contexts and with an update to the
real-cluster compatibility checks.

## Terms

- **Configured contexts** are all source contexts selected for the proxy.
- **Primary context** is the explicitly configured compatibility context.
- **Configured order** is the stable order in the proxy state.  It is used only
  when a rule explicitly asks for a deterministic non-primary tie-breaker.
- **Object path** identifies one Kubernetes object, for example
  `/apis/apps/v1/namespaces/{namespace}/deployments/{name}`.
- **Collection path** addresses a resource collection, with or without a
  namespace, for example `/api/v1/pods` or
  `/api/v1/namespaces/{namespace}/pods`.
- **Success** is any `2xx` upstream response.  A `404` is an expected absence
  only while locating an existing named object.

`{group}`, `{version}`, `{namespace}`, `{resource}`, `{name}`, and
`{subresource}` below are placeholders.  A rule for `/api/...` applies to the
core API; the corresponding `/apis/{group}/{version}/...` form has the same
meaning unless noted otherwise.

## Precedence

Classify a request once, from top to bottom.  A later rule must not override an
earlier one.

| Priority | Request class | Target selection | Client-visible result |
| --- | --- | --- | --- |
| 1 | Missing or invalid local bearer token | None | `401 Unauthorized`. |
| 2 | `POST`, `PUT`, `PATCH`, or `DELETE` with a read-only proxy | None | `403 Forbidden` before any upstream call. |
| 3 | Discovery and non-resource compatibility endpoints | Primary context | Forward its response unchanged. |
| 4 | Authentication, authorization, and token request APIs | Primary context | Forward its response unchanged. |
| 5 | Helm release-storage list or watch when Helm compatibility mode is enabled | Primary context | Forward or stream only the primary response. |
| 6 | Watch | Every configured context | Open and merge streams. |
| 7 | Pod connection subresource | Context containing the Pod | Stream one upstream connection. |
| 8 | Named object `GET` | Every configured context | Return a found object, preferring primary. |
| 9 | Collection `GET` without `watch=true` | Every configured context | Aggregate the collection. |
| 10 | Persistent-resource mutation | Annotation, existing object, or every configured context | Complete the mutation according to the mutation rules. |
| 11 | All other requests | Primary context | Forward its response unchanged. |

The final primary-only rule is deliberate.  An unknown endpoint must never
become a fan-out mutation merely because it is unfamiliar.  Adding a new
multi-context class requires a matrix row and target-selection tests first.

## Request Groups

### Discovery and non-resource endpoints

| Path examples | Target | Notes |
| --- | --- | --- |
| `/api`, `/api/{version}`, `/apis`, `/apis/{group}`, `/apis/{group}/{version}` | Primary | Discovery must describe one coherent API surface. |
| `/version`, `/openapi/*`, `/swagger/*`, `/healthz`, `/livez`, `/readyz` | Primary | `kubectl explain`, discovery caches, and health clients receive a coherent answer. |
| Any unclassified non-resource path | Primary | Safe compatibility fallback. |

### Authentication, authorization, and ephemeral credentials

| Path examples | Target | Reason |
| --- | --- | --- |
| `POST /apis/authentication.k8s.io/{version}/selfsubjectreviews` | Primary | A response describes the primary credential's identity; there is no Kubernetes aggregate response type. |
| `POST /apis/authorization.k8s.io/{version}/selfsubjectaccessreviews` and other access-review APIs | Primary | `kubectl auth can-i` expects one boolean response. |
| `POST .../serviceaccounts/{name}/token` and TokenRequest APIs | Primary | A token is cluster-specific and must not be minted in every context or returned arbitrarily. |

These are request/response operations, not ordinary persistent-resource
creation.  They must not use generic mutation fan-out.

### Lists and pagination

| Request pattern | Target | Result |
| --- | --- | --- |
| `GET {collection-path}` without `watch=true`, `limit`, or a proxy continuation token | All contexts concurrently | All upstream responses must succeed.  Merge Kubernetes `items` or `Table.rows`, add the source annotation and virtual label to every returned object, and return one aggregate resource version. |
| Same request with `limit` or an aggregate continuation token | All contexts, sequentially as required by the aggregate continuation state | Enforce one global limit and return only an opaque proxy continuation token.  Never forward a context-local token to another context. |
| Collection list for Helm release Secrets or ConfigMaps in Helm compatibility mode | Primary | Do not merge independent release histories. |

For an aggregate list, a transport failure or non-success response from any
context fails the request.  The error must name the context.  The proxy must
not silently return a partial list.

### Watches

| Request pattern | Target | Result |
| --- | --- | --- |
| `GET {collection-path}?watch=true` | All contexts concurrently | Open one watch per context and forward events as they arrive.  Add the source annotation and virtual label to event objects.  Ordering between contexts is intentionally undefined. |
| Named object watch (`GET {object-path}?watch=true`) or collection watch with `fieldSelector=metadata.name={name}` | Contexts containing the named object | First locate the object in every context.  Open one watch only for each found object, using its context-local resource version.  A `404` is an expected absence during the lookup. |
| Helm release-storage watch in Helm compatibility mode | Primary | Stream one release history only. |

The initial upstream-open failure must identify its context.  Successful streams
are not buffered and are not subject to the ordinary request timeout.
Identically named objects from separate contexts retain their ordinary
Kubernetes identity (`namespace` and `name`); source markers do not create a
distinct client-side cache key.  A named watch, including `kubectl rollout
status`, is therefore not a cross-context readiness barrier.

### Named object reads

| Request pattern | Target | Result |
| --- | --- | --- |
| `GET {object-path}` | All contexts concurrently | If primary returns `2xx`, return that response.  Otherwise return the first `2xx` response in configured order. |

If no context returns `2xx` and all contexts return `404`, return the primary
context's normal `404` response.  If no object is found and any context has a
transport error or a non-`404` response, return a context-named failure rather
than representing the result as an ordinary absence.  A found object wins over
a `404` or failure from another context: this is a presence lookup, not an
aggregate read.

### Pod connection subresources

| Request pattern | Target | Result |
| --- | --- | --- |
| `GET .../pods/{name}/log`, `exec`, `attach`, or `portforward` | Context containing the Pod | Locate the Pod across contexts, preferring primary if it exists there, then proxy the one streaming connection. |
| `kubectl cp` | Same as `exec` | `kubectl cp` uses the Pod `exec` subresource. |

If the Pod is absent everywhere, forward the connection request to primary so
the client receives the ordinary Kubernetes error.  The proxy must not open a
connection to every context.

### Persistent-resource mutations

This group includes `POST` to a collection, `PUT`, `PATCH`, and `DELETE` for a
Kubernetes resource, including cluster-scoped resources.  The target is chosen
in this order:

1. `metadata.annotations["kubeconfig-proxy.io/context-name"]` in the request
   object selects exactly that configured context.  An unknown name is a local
   `400` and makes no upstream call.
2. Otherwise, `metadata.annotations["kubeconfig-proxy.io/single-context"] =
   "true"` selects the primary context.
3. For a named `PATCH`, `DELETE`, or update whose body has no routing
   annotation, read the existing object in every context first.  Its routing
   annotation takes precedence.  Without one, route to the contexts where the
   object exists; if it exists everywhere, this is normal fan-out.
4. For an object-associated subresource mutation (for example `scale`,
   `status`, `finalize`, or Pod `ephemeralcontainers`), apply the same lookup
   to the owning object before routing the subresource request.
5. Otherwise fan out to every configured context.

The owning-object lookup also applies to a named `PUT` when the body does not
carry a routing annotation.  For each selected target, a `PUT` must use that
target's current `uid` and `resourceVersion`; identity from one cluster must
never be sent to another.

For fan-out mutations, send all selected requests concurrently and wait for all
of them.  On a failure, return a Kubernetes `Status` failure that identifies
the failing context and preserves the useful upstream reason where possible.
There is no rollback: a request can succeed in some contexts before another
context fails.  If every selected request succeeds, return the successful
response from primary when it was selected; otherwise use the first selected
context in configured order.  Never select the representative response by
goroutine completion order.

Collection deletion and other mutations that do not identify one existing
object cannot use existing-object annotations; absent a request-body routing
annotation, they fan out to every configured context.

## Response, Error, and Concurrency Rules

- Multi-context list reads, named-object probes, mutation fan-out, and watch
  opening start one upstream request per selected context concurrently.
- A context name is required in every proxy-generated upstream failure.  Do not
  return an arbitrary upstream error without saying which context produced it.
- A context-local `404` is only an expected absence while locating a named
  object or Pod.  It is not silently ignored for aggregate lists or fan-out
  mutations.
- The proxy must preserve request method, path, query parameters, Kubernetes
  content negotiation, and upstream authentication.  It removes only the
  local proxy bearer token before forwarding.
- List and watch merging may rewrite objects only to add
  `kubeconfig-proxy.io/context` and the virtual `context` label, plus opaque
  aggregate pagination/resource-version values.  Other fields remain upstream
  data.
- Read-only rejection, body-size limits, retry policy, and TLS/authentication
  boundaries apply before or during routing exactly as documented elsewhere;
  no routing rule weakens them.

## Development Workflow

1. Refresh the command corpus with the `gen-kubectl-commands` skill when the
   installed `kubectl` version or its covered command set changes.
2. Normalize each new observed request to a method, resource shape, query
   mode, and subresource.  Discovery requests captured before a command are
   classified independently from the command's resource request.
3. Match it to an existing row or add a new row here before changing routing
   code.  Specify targets, concurrency, response selection, and failure
   semantics; do not use a generic fallback as an undocumented policy.
4. Add unit tests that assert the exact contexts contacted and the response
   chosen, including partial-success cases for mutations and presence cases for
   named reads.
5. Add or update real-cluster checks for user-visible behaviour, then run the
   smallest proxy test package before the full project checks.

The corpus is a compatibility input, not an exhaustive Kubernetes API
specification.  In particular, CRDs, aggregated APIs, and newly introduced
subresources are covered by the generic resource-shape rules only after their
semantics have been reviewed.
