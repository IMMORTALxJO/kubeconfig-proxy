---
name: gen-kubectl-commands
description: Write an ordered kubectl command corpus directly to YAML for capturing Kubernetes API HTTP requests. Use when preparing a disposable test cluster for kubectl-to-HTTP tracing, refreshing commands after a kubectl upgrade, or needing API-oriented kubectl invocations with representative argument variants.
---

# Generate Kubectl Commands

Inspect the installed client's `kubectl --help` and the `--help` page of every
selected command or subcommand. Then directly update
`.codex/skills/gen-kubectl-commands/kubectl-commands.yaml`; do not create a
command-generation script.

## Build the sequence

Use only a disposable cluster. Start by deleting and recreating namespace
`kcp-http-corpus`, create all test objects there, and delete the namespace at
the end. Do not execute the listed commands while generating the file.

Include every built-in command printed by `kubectl --help`, recursively
including its built-in subcommands. Exclude only commands shown under
"Subcommands provided by plugins". Keep commands that are client-only or that
are expected to return an API error: they belong in the corpus and may have an
empty `requests` list. For commands that accept different forms of arguments,
include useful distinct invocations: for example, all-namespaces versus one
namespace, list versus named object, label selector, output type, repeated
literals, and merge versus JSON patches. Keep setup commands before consumers
and cleanup commands last.

Cover streaming and interactive API paths too: `logs`, `exec`, `attach`,
`port-forward`, `proxy`, `cp`, and `debug`. The collector deliberately limits
commands that keep a connection open; retain the requests captured before the
limit expires. Do not invent a variant that the installed help text does not
advertise.

## Output format

Keep this file as a YAML sequence with no wrapper keys or generated metadata:

```yaml
- command: "kubectl get pods --all-namespaces"
  requests:
    - method: GET
      url: https://127.0.0.1:6443/api/v1/pods
      args: {"limit": ["500"]}
      body: ""
```

Initially set `requests` to `[]`. Later collection adds every observed HTTP
exchange to the matching entry in order; retain discovery, authentication, and
retry requests instead of collapsing them into the final resource request.
Store a textual body verbatim; a non-text body is encoded as a string prefixed
with `base64:`.

## Capture the requests

Run the bundled collector from the repository root after writing the command
list:

```bash
GOTOOLCHAIN=auto go run .codex/skills/gen-kubectl-commands/scripts/capture.go
```

It creates a uniquely named temporary kind cluster, starts a short-lived
warm-up pod and waits until its workload image is pulled before running the
corpus, starts a local TLS recording proxy, configures a temporary kubeconfig to send `kubectl` through
that proxy, executes the entries sequentially, and writes each captured request
back to `kubectl-commands.yaml`. The collector forwards requests to the kind
API server using the original kind credentials; it never writes authorization
headers to the YAML. It deletes only the kind cluster it created, including
when a command fails.

Do not parse `kubectl -v` output. It is diagnostic text rather than a stable
machine interface and may omit or reformat request bodies. Continue after a
command failure or a controlled stream timeout: preserve every request captured
for that entry and report the failed command after the corpus completes.
