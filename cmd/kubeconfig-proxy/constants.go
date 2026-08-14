package main

import (
	"errors"
	"time"
)

const readinessPath = "/-/kubeconfig-proxy/ready"

const statePollInterval = time.Second

var (
	errStateFileChanged        = errors.New("state file changed")
	errSourceKubeconfigChanged = errors.New("source kubeconfig changed")
	errStateFileRemoved        = errors.New("state file removed")
)
