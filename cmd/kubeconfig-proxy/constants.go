package main

import (
	"errors"
	"time"
)

const readinessPath = "/-/kubeconfig-proxy/ready"

const credentialStartupTimeout = time.Minute

const statePollInterval = time.Second

// Let in-place writers such as client-go finish before replacing the process.
const runtimeFileSettleInterval = 50 * time.Millisecond

// Let multi-request clients issue follow-up requests before replacing the process.
const runtimeReloadIdleDelay = 5 * time.Second

var (
	errStateFileChanged        = errors.New("state file changed")
	errSourceKubeconfigChanged = errors.New("source kubeconfig changed")
	errStateFileRemoved        = errors.New("state file removed")
)
