# Routing Contract

This document records the routing matrix implemented by `kubeconfig-proxy` as
an HTTP API in front of several Kubernetes contexts.  The source code and its
tests are authoritative; this document must be updated whenever their behaviour
changes.  `kubectl` compatibility is the first constraint: each implemented
request class has an explicit target-selection and response rule.

The locally generated corpus at
`.codex/skills/gen-kubectl-commands/kubectl-commands.yaml` is a workflow input
for refreshing the matrix.  It is intentionally ignored by Git and may be
absent from a clean checkout.  The rules below are path-pattern rules, not a
closed list of resources from one captured corpus, so they also apply to custom
resources and API groups not present in the latest local capture.

Documentation alone does not change proxy behaviour.  Every implementation
change must be delivered with unit tests that assert the exact upstream
contexts, an update to this matrix, and an update to the real-cluster
compatibility checks when user-visible behaviour changes.

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
| 4 | Recognized authentication, authorization, and token request APIs | Primary context | Forward its response unchanged. |
| 5 | Matching Helm storage list or watch when Helm compatibility mode is enabled | Primary context | Forward or stream only the primary response. |
| 6 | Watch | Every configured context, or only contexts containing a named object | Open and merge the selected streams. |
| 7 | Pod connection subresource | Context containing the Pod, or primary when it is absent everywhere | Stream one upstream connection. |
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

### Recognized request/response and token APIs

| Path examples | Target | Reason |
| --- | --- | --- |
| Any `POST` path containing `selfsubjectreview`, `accessreview`, or `tokenreviews` | Primary | These path substrings are recognized before generic resource mutation routing. |
| `POST` ending in `/serviceaccounts/token` or containing `/serviceaccounts/` and ending in `/token` | Primary | These paths are treated as cluster-specific token requests. |

Matching is intentionally the literal path-substring and suffix logic above.
For example, `SelfSubjectRulesReview` does not contain any recognized substring
and therefore follows the generic persistent-resource mutation route.

### Lists and pagination

| Request pattern | Target | Result |
| --- | --- | --- |
| `GET {collection-path}` without `watch=true`, `limit`, or a proxy continuation token | All contexts concurrently | All upstream responses must succeed.  Merge Kubernetes `items` or `Table.rows`, add the source annotation and virtual label to every returned object, and return one aggregate resource version. |
| Same request with a positive `limit` up to `10000`, optionally with an aggregate continuation token | All contexts, sequentially as required by the aggregate continuation state | Enforce one global limit and return only an opaque proxy continuation token.  Never forward a context-local token to another context. |
| Request with an aggregate continuation token but no positive `limit` | None when `limit` is absent or invalid; all contexts when `limit=0` | A missing or invalid limit returns local `400`.  `limit=0` removes both pagination parameters and performs an ordinary unpaginated aggregate list. |
| Secret or ConfigMap collection list whose decoded `labelSelector` contains `owner=helm` or `owner==helm`, in Helm compatibility mode | Primary | The literal substring match prevents merging independent release histories. |

For an aggregate list, a transport failure or non-success response from any
context fails the request.  The error must name the context.  The proxy must
not silently return a partial list.

### Watches

| Request pattern | Target | Result |
| --- | --- | --- |
| `GET {collection-path}?watch=true` | All contexts concurrently | Establish a resource-version position for every context, open one watch per context, and forward events as they arrive.  Add the source annotation and virtual label to event objects.  Every event carries the updated opaque aggregate resource version.  Ordering between contexts is intentionally undefined. |
| Named object watch (`GET {object-path}?watch=true`) or collection watch with `fieldSelector=metadata.name={name}` | Contexts containing the named object | First locate the object in every context.  Open one watch only for each found object, using its context-local resource version.  A `404` is an expected absence during the lookup. |
| Secret or ConfigMap collection watch whose decoded `labelSelector` contains `owner=helm` or `owner==helm`, in Helm compatibility mode | Primary | Stream one matching history only. |

When `resourceVersion` is absent, the proxy first captures the current list
resource version from every selected context.  Every forwarded event
contains the latest complete vector; only the coordinate for the event's source
context advances.  If any upstream watch ends, the proxy cancels the remaining
upstream watches and closes the downstream stream instead of continuing with a
partial set of contexts.  A malformed aggregate resource version, or one that
does not contain every selected context, returns local `400 Bad Request` before
any upstream watch is opened.

The initial upstream-open failure must identify its context.  Successful streams
are not buffered and are not subject to the ordinary request timeout.
Identically named objects from separate contexts retain their ordinary
Kubernetes identity (`namespace` and `name`); source markers do not create a
distinct client-side cache key.  A named watch, including `kubectl rollout
status`, is therefore not a cross-context readiness barrier.

### Named object reads

| Request pattern | Target | Result |
| --- | --- | --- |
| `GET {object-path}` | All contexts concurrently | If primary returns `2xx`, return that response. Otherwise return the first `2xx` response in configured order. Set `metadata.labels["kcp-context"]` to the comma-separated configured contexts that returned `2xx`. |

If no context returns `2xx` and all contexts return `404`, return a
proxy-generated Kubernetes `Status` with `404` and the first configured
context's name.  If no object is found and any context has a transport error or
a non-`404` response, return a context-named failure rather than representing
the result as an ordinary absence.  A found object wins over a `404` or failure
from another context: this is a presence lookup, not an aggregate read.

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

1. `metadata.annotations["kubeconfig-proxy.io/target-context"]` in the request
   body selects the comma-separated configured contexts. Surrounding whitespace
   is ignored, repeated names are selected once, and an empty or unknown name
   is a local `400` before any upstream call.
2. Otherwise, `metadata.annotations["kubeconfig-proxy.io/single-context"] =
   "true"` selects the primary context.
3. For a named `PATCH`, `DELETE`, or update whose body has no routing
   annotation, read the existing object in every context first.  Its routing
   annotation takes precedence.  Without one, route to the contexts where the
   object exists; if it exists everywhere, this is normal fan-out.  An empty or
   unknown target in an existing object returns local `400` after the lookup and
   before any mutation call.
4. For an object-associated subresource mutation (for example `scale`,
   `status`, `finalize`, or Pod `ephemeralcontainers`), apply the same lookup
   to the owning object before routing the subresource request.
5. Otherwise fan out to every configured context.

The owning-object lookup also applies to a named `PUT` when the body does not
carry a routing annotation.  For each selected target, a `PUT` must use that
target's current `uid` and `resourceVersion`; identity from one cluster must
never be sent to another.

For fan-out mutations, send all selected requests concurrently and wait for all
of them.  A transport failure returns a proxy-generated `502` Kubernetes
`Status` naming the context.  A non-`2xx` upstream response returns a
proxy-generated Kubernetes `Status` with the upstream HTTP code and status text;
the upstream response body is not preserved.  There is no rollback: a request
can succeed in some contexts before another context fails.  If every selected
request succeeds, return the successful response from primary when it was
selected; otherwise use the first selected context in configured order.  Never
select the representative response by goroutine completion order.

Collection deletion and other mutations that do not identify one existing
object cannot use existing-object annotations; absent a request-body routing
annotation, they fan out to every configured context.

## Response, Error, and Concurrency Rules

- Multi-context list reads, named-object probes, mutation fan-out, and watch
  opening start one upstream request per selected context concurrently.
- A context name is required in every proxy-generated upstream failure.  Do not
  return an arbitrary upstream error without saying which context produced it.
- Proxy-generated upstream failures do not copy the upstream response body.
- A context-local `404` is only an expected absence while locating a named
  object or Pod.  It is not silently ignored for aggregate lists or fan-out
  mutations.
- The proxy must preserve request method, path, query parameters, Kubernetes
  content negotiation, and upstream authentication.  It removes only the
  local proxy bearer token and removes the virtual `kcp-context` label from
  ordinary manifest bodies sent with `POST` or `PUT`. `PATCH` bodies are
  preserved unchanged.
- List and watch merging may rewrite objects only to add
  `kubeconfig-proxy.io/source-context` and the virtual `kcp-context` label, plus opaque
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
