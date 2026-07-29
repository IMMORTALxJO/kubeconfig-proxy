package main

import (
	"crypto/sha256"
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

func resolveDefaultKubeconfigPath() string {
	return clientcmd.NewDefaultClientConfigLoadingRules().GetDefaultFilename()
}

func resolveDefaultStatePath(contextName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("detect home dir: %w", err)
	}
	return filepath.Join(home, ".kube", "kubeconfig-proxy", sanitizeFileName(contextName)+".yaml"), nil
}

func sanitizeFileName(value string) string {
	const maxReadablePrefix = 80

	safe := strings.Map(func(r rune) rune {
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
	changed := safe != value
	if len(safe) > maxReadablePrefix {
		safe = safe[:maxReadablePrefix]
		changed = true
	}
	if !changed {
		return safe
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s-%x", safe, sum[:6])
}

func resolveDefaultExecCommand() string {
	path, err := os.Executable()
	if err != nil {
		return "kubeconfig-proxy"
	}
	return path
}

func formatDurationForLog(value time.Duration) string {
	if value <= 0 {
		return "disabled"
	}
	return value.String()
}
