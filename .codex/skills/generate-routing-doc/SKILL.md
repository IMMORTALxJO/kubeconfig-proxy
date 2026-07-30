---
name: generate-routing-doc
description: Generate or revise the English ROUTING.md contract for kubeconfig-proxy from the captured kubectl HTTP corpus. Use when grouping kubectl API requests, deciding target selection and response semantics, refreshing routing documentation after kubectl coverage changes, or reviewing a new Kubernetes API path before implementing proxy routing.
---

# Generate Routing Doc

Create `ROUTING.md` as the explicit, reviewable routing contract for the
multi-context Kubernetes API proxy. Treat transparent `kubectl` behaviour and
safe mutation routing as the primary constraints.

## Inputs

Read these files before drafting:

1. Repository `AGENTS.md` instructions and `ARCHITECTURE.md`.
2. `.codex/skills/gen-kubectl-commands/kubectl-commands.yaml`.
3. `internal/proxy/proxy.go`, `routing.go`, `mutation.go`, `forward.go`,
   `aggregate.go`, and `watch.go`, plus their focused tests.
4. `README.md` and any existing `ROUTING.md`.

If the corpus is absent, empty, or must be refreshed for a changed `kubectl`
version, use the `gen-kubectl-commands` skill first. Do not infer API traffic
from `kubectl -v` output. Preserve the corpus's discovery, authentication, and
retry exchanges while analysing it, even when they do not become application
resource routes.

## Analyse the Corpus

Parse the YAML structurally. Count commands and HTTP exchanges, then normalize
each exchange by method, resource shape, query mode, and subresource. Replace
only variable values with placeholders such as `{group}`, `{version}`,
`{namespace}`, `{resource}`, `{name}`, and `{subresource}`.

Keep these distinctions separate:

- discovery, OpenAPI, health, and other non-resource endpoints;
- collection `GET`, paginated collection `GET`, and `watch=true`;
- named-object `GET` and existence probes;
- streaming Pod subresources (`log`, `exec`, `attach`, `portforward`) and the
  `kubectl cp` path that uses `exec`;
- persistent-resource creates, updates, patches, deletes, collection deletes,
  and object-associated subresource mutations;
- request/response `POST` APIs such as access reviews and TokenRequests, which
  do not create a normally routable persistent object;
- client-only commands that have no requests.

Do not group solely by HTTP verb. In particular, `POST` to a resource
collection and `POST` for an access review have different target and response
semantics.

## Decide and Write the Contract

Write `ROUTING.md` strictly in English. Use readable tables and normalized path
examples; do not copy raw loopback hosts, request bodies, authorization data,
or corpus tokens into it.

The document must contain:

1. **Purpose and evidence**: identify the corpus and its command/request
   counts, and say whether the document describes current behaviour or a target
   contract.
2. **Terms**: configured contexts, primary context, configured order, object
   path, collection path, and the meaning of success and expected absence.
3. **A precedence table**: every request must have one ordered class and an
   explicit safe primary-only fallback for unknown endpoints.
4. **Request-group tables**: state target selection, concurrency, response
   choice, and failure handling for discovery, request/response APIs, lists and
   pagination, watches, named reads, streaming subresources, and mutations.
5. **Mutation precedence**: state the exact order of
   `kubeconfig-proxy.io/context-name`,
   `kubeconfig-proxy.io/single-context`, existing-object routing, and default
   fan-out. Record the user-approved meaning of `single-context`; do not infer
   it from an older implementation.
6. **Response and error rules**: require context names in proxy-generated
   failures, define when a `404` is an expected absence, state that successful
   fan-out waits for all selected contexts, and prohibit hidden partial list
   results or mutation rollback claims.
7. **Development workflow**: corpus refresh, classification, contract update,
   exact-target unit tests, real-cluster checks, and proportionate validation.

Prefer generic Kubernetes resource-shape rules that cover CRDs and aggregated
APIs after review. Explicitly document exceptions that must remain primary-only
for compatibility, including discovery and APIs whose response cannot be
meaningfully aggregated.

When a proposed rule differs from the current code, call it a target contract
and list the conformance work needed. Do not present unimplemented behaviour as
already shipped. Ask the user to decide only when the choice changes real
cluster side effects or the public routing contract.

## Keep Documentation Coherent

Link the detailed matrix from `README.md`. Keep `ARCHITECTURE.md` consistent
with the matrix's routing order and package responsibilities, without copying
the whole table unnecessarily. Update user-visible annotation explanations
when their semantics change.

Do not modify runtime Go code unless the user also requests implementation.
Do not weaken read-only checks, authentication, TLS, body limits, retries, or
the existing source-context markers.

## Validate

Before handing off:

1. Parse `kubectl-commands.yaml` and report non-zero command and request
   counts.
2. Confirm `ROUTING.md` contains only English prose and contains no secrets or
   raw corpus bodies.
3. Run `git diff --check`.
4. Review the changed routing documentation against the current proxy decision
   tree and clearly report any intentional implementation gaps.
