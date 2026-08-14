package main

import (
	"errors"
	"time"
)

const readinessPath = "/-/kubeconfig-proxy/ready"

const statePollInterval = time.Second

// Let in-place writers such as client-go finish before replacing the process.
const runtimeFileSettleInterval = 50 * time.Millisecond

var (
	errStateFileChanged        = errors.New("state file changed")
	errSourceKubeconfigChanged = errors.New("source kubeconfig changed")
	errStateFileRemoved        = errors.New("state file removed")
)
