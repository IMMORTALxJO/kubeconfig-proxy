package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
)

type deleteContextOptions struct {
	contextName    string
	kubeconfigPath string
	statePath      string
}

func runDeleteContext(args []string) error {
	options, err := parseDeleteContextOptions(args)
	if err != nil {
		return err
	}
	absoluteKubeconfigPath, statePaths, err := deleteContextFiles(options)
	if err != nil {
		return err
	}
	if err := removeStateArtifacts(statePaths); err != nil {
		return err
	}

	log.Printf("updated kubeconfig: %q", absoluteKubeconfigPath) // #nosec G706 -- %q escapes control characters in the user-provided kubeconfig path.
	log.Printf("deleted context:    %q", options.contextName)    // #nosec G706 -- %q escapes control characters in user-provided context names.
	for _, statePath := range statePaths {
		log.Printf("deleted state:      %q", statePath) // #nosec G706 -- %q escapes control characters in local state paths.
	}
	return nil
}

func parseDeleteContextOptions(args []string) (deleteContextOptions, error) {
	flags := flag.NewFlagSet("kubeconfig-proxy delete-context", flag.ContinueOnError)
	contextName := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		contextName = args[0]
		args = args[1:]
	}
	var (
		kubeconfigPath = flags.String("kubeconfig", resolveDefaultKubeconfigPath(), "kubeconfig path to update")
		statePath      = flags.String("state", "", "additional state file path to remove")
	)
	if err := flags.Parse(args); err != nil {
		return deleteContextOptions{}, err
	}
	contextName, err := resolveDeleteContextName(contextName, flags.Args())
	if err != nil {
		return deleteContextOptions{}, err
	}
	return deleteContextOptions{
		contextName:    contextName,
		kubeconfigPath: *kubeconfigPath,
		statePath:      *statePath,
	}, nil
}

func resolveDeleteContextName(contextName string, positionalArgs []string) (string, error) {
	if contextName != "" && len(positionalArgs) == 0 {
		return contextName, nil
	}
	if contextName == "" && len(positionalArgs) == 1 {
		return positionalArgs[0], nil
	}
	return "", fmt.Errorf("usage: kubeconfig-proxy delete-context <context-name> [flags]")
}

func deleteContextFiles(options deleteContextOptions) (string, []string, error) {
	absoluteKubeconfigPath, err := filepath.Abs(options.kubeconfigPath)
	if err != nil {
		return "", nil, err
	}
	statePaths, err := kubeconfig.DeleteProxyContext(absoluteKubeconfigPath, options.contextName)
	if err != nil {
		return "", nil, err
	}
	statePaths, err = appendExplicitStatePath(statePaths, options.statePath)
	if err != nil {
		return "", nil, err
	}
	if len(statePaths) == 0 {
		defaultPath, err := resolveDefaultStatePath(options.contextName)
		if err != nil {
			return "", nil, err
		}
		statePaths = append(statePaths, defaultPath)
	}
	return absoluteKubeconfigPath, statePaths, nil
}

func appendExplicitStatePath(statePaths []string, statePath string) ([]string, error) {
	if statePath == "" {
		return statePaths, nil
	}
	absoluteStatePath, err := filepath.Abs(statePath)
	if err != nil {
		return nil, err
	}
	return kubeconfig.AppendUniquePaths(statePaths, absoluteStatePath), nil
}

func removeStateArtifacts(statePaths []string) error {
	for _, statePath := range statePaths {
		for _, path := range []string{statePath, statePath + ".log"} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) { // #nosec G703 -- delete-context removes only the managed state file paths recorded in kubeconfig.
				return err
			}
		}
	}
	return nil
}
