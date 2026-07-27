package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func defaultKubeconfigPath() string {
	return clientcmd.NewDefaultClientConfigLoadingRules().GetDefaultFilename()
}

func defaultStatePath(contextName string) string {
	return filepath.Join(mustHomeDir(), ".kube", "kubeconfig-proxy", safeFileName(contextName)+".yaml")
}

func safeFileName(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, value)
}

func defaultExecCommand() string {
	path, err := os.Executable()
	if err != nil {
		return "kubeconfig-proxy"
	}
	return path
}

func mustHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Sprintf("detect home dir: %v", err))
	}
	return home
}

func durationLogValue(value time.Duration) string {
	if value <= 0 {
		return "disabled"
	}
	return value.String()
}
