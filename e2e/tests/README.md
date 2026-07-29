# Custom e2e checks

Add Bash scripts named `test_*` to this directory. `../run.sh` executes each
regular file after starting the two-cluster proxy and streams its output to the
terminal. A script passes only when it exits with status `0`.

The runner provides `KUBECTL_BIN`, `KCP_BIN`, `KUBECONFIG`, `CONTEXT_PROXY`,
`CONTEXT_A`, `CONTEXT_B`, and `NAMESPACE` as environment variables.
`NAMESPACE` is the stable `kubeconfig-proxy-e2e-tests` namespace name for test
resources; each custom test owns its namespace lifecycle. The temporary
kubeconfig is removed after the run unless `KCP_KEEP_KIND=1` is set.
