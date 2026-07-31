# Repository Guidelines

## Project Context

`kubeconfig-proxy` is a Go CLI and local Kubernetes API proxy. It writes managed kubeconfig contexts, starts an HTTPS local proxy through exec credentials, and fans out or aggregates Kubernetes API requests across selected source contexts.

Treat routing behavior as the core product contract. Small changes in request classification can affect real clusters, so prefer stable, explicit code over clever abstractions.

## Engineering Principles

- Prefer stability, security, and simple code over broad refactors.
- Keep changes tightly scoped to the behavior being requested.
- Preserve existing public CLI flags, state file fields, annotations, and README promises unless the user explicitly asks for a breaking change.
- Avoid hidden magic in routing logic. Make target selection easy to read and easy to test.
- Do not weaken authentication, TLS handling, state file permissions, read-only mode, or `gosec` suppressions.
- Do not log bearer tokens, private keys, kubeconfig contents, or full state files.
- Work with user changes already present in the tree. Do not revert unrelated edits.

## Development Workflow

- Follow TDD: write or update focused unit tests before implementing the production code.
- After the implementation and tests are complete, run a code review and fix all identified issues before handing off the change.

## Go Practices

- Use the Go version declared in `go.mod`; project commands set `GOTOOLCHAIN` through the `Makefile`.
- Format Go files with `gofmt`; do not hand-format.
- Prefer standard library and existing project helpers before adding dependencies.
- Keep error messages actionable and include target/context names when diagnosing upstream calls.
- Use structured Kubernetes/client-go APIs where practical. Avoid ad hoc parsing for kubeconfig, YAML, or JSON when a local helper already exists.
- Add small helpers only when they remove real duplication or protect shared behavior. Do not introduce framework-style abstractions for one-off code.
- Keep comments rare and useful; explain non-obvious routing or security decisions, not ordinary assignments.

### Naming Conventions

- Use idiomatic Go `camelCase` for local variables and unexported functions; use `PascalCase` only for exported identifiers.
- Choose short names only for small, obvious scopes (`ctx`, `err`, `req`, `resp`, `cfg`). Else, prefer descriptive names that state the value's role, such as `sourceContext`, `targetClient`, or `routingAnnotations`.
- Name booleans as readable predicates: `isReadOnly`, `hasContextName`, `shouldFanOut`, `canMutate`. Avoid vague boolean names such as `flag`, `enabled`, or `value`.
- Name collections as plural nouns (`contexts`, `targets`, `errors`) and maps for their lookup role (`clientsByContext`, `annotationsByName`).
- Name functions with a verb that describes their effect or result: `loadState`, `selectTarget`, `validateRequest`, `writeResponse`. Predicate functions should start with `is`, `has`, `can`, `should`, or `needs`.
- Keep names aligned with the domain vocabulary already used by the CLI and proxy: use `context` for a configured Kubernetes context, `target` for a selected upstream, and `route` or `routing` for dispatch decisions.
- Avoid abbreviations unless they are established Go or Kubernetes terms. Do not encode types in names (`strName`, `contextMap`) or repeat enclosing type/package names unnecessarily.

## Proxy Behavior Rules

- Read/list/watch behavior must keep source context markers intact:
  - annotation: `kubeconfig-proxy.io/source-context`
  - virtual label: `kcp-context`
- Mutations are dangerous. Tests should cover where a request is sent, not only the final HTTP status.
- `kubeconfig-proxy.io/target-context` must route mutations to the named configured context.
- `kubeconfig-proxy.io/single-context` must route mutations to the alphabetically first configured context unless `target-context` is present.
- Named `PATCH` and `DELETE` must consider the existing object when request bodies do not carry routing annotations.
- Read-only proxy contexts must reject `POST`, `PUT`, `PATCH`, and `DELETE` before upstream calls.
- Discovery and primary-only behavior must stay predictable, especially for Helm/werf release storage compatibility.
- Long-running pod subresources (`log`, `exec`, `attach`, `portforward`) should route to the cluster that contains the pod.

## Testing

- Do not modify e2e tests unless the user explicitly requests it.

Run the smallest relevant test first, then broaden:

```bash
GOTOOLCHAIN=auto go test ./internal/proxy
GOTOOLCHAIN=auto go test ./...
make check
```

Before handing off meaningful code changes, prefer `make check`. It runs formatting checks, `go vet`, `staticcheck`, `gosec`, `govulncheck`, unit tests, race tests for `internal/proxy`, and build.

For real-cluster integration coverage, use the repository skill:

```bash
e2e/run.sh
```

When adding new user-visible proxy behavior, add or update checks in
`.codex/skills/test-kubeconfig-proxy` so `/test-kubeconfig-proxy` verifies the
feature against real `kubeconfig-proxy-a` and `kubeconfig-proxy-b` kind
clusters. Unit tests are still required; the skill is the integration safety net
for Kubernetes API behavior.

Useful options:

- `KCP_SKIP_MAKE_CHECK=1` reuses `bin/kubeconfig-proxy` for faster integration reruns.
- `KCP_KEEP_KIND=1` keeps temporary files and kind clusters for debugging.
- `KCP_RECREATE_KIND=1` recreates `kubeconfig-proxy-a` and `kubeconfig-proxy-b`.

## Local Environment Safety

- Integration tests use local kind clusters named `kubeconfig-proxy-a` and
  `kubeconfig-proxy-b`, pinned to Kubernetes `v1.36.1`.
- Do not run destructive Kubernetes commands outside the temporary kubeconfig or those explicit kind contexts.
- Clean up test resources when adding new integration checks.
- Avoid changing the user's default kubeconfig unless the task explicitly requires it. Prefer temporary kubeconfig files for tests and examples.
- Do not delete existing kind clusters unless the user asks or an integration script option clearly requests recreation.

## Documentation

- Update `README.md` or examples when user-visible CLI behavior changes.
- Keep examples executable and aligned with the current binary name, context names, and flags.
- When adding a feature, document the expected routing semantics and the safety boundary.
- Treat `ARCHITECTURE.md` as the source of truth for package responsibilities,
  dependency direction, runtime lifecycle, routing ownership, state boundaries,
  and repository layout.
- Update `ARCHITECTURE.md` in the same change whenever those architectural
  decisions or the documented layout change. Do not leave architecture changes
  documented only in code, a pull request, or `README.md`.
