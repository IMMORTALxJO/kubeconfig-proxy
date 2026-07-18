package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
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

func targetNames(targets []proxy.Target) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	return names
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func appendUniqueStrings(values []string, more ...string) []string {
	for _, value := range more {
		if value == "" || contains(values, value) {
			continue
		}
		values = append(values, value)
	}
	return values
}

func defaultKubeconfigPath() string {
	if value := os.Getenv("KUBECONFIG"); value != "" {
		return filepath.SplitList(value)[0]
	}
	return filepath.Join(mustHomeDir(), ".kube", "config")
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
