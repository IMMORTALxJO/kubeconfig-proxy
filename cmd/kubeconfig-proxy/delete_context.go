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

func runDeleteContext(args []string) error {
	flags := flag.NewFlagSet("kubeconfig-proxy delete-context", flag.ContinueOnError)
	contextName := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		contextName = args[0]
		args = args[1:]
	}
	var (
		kubeconfigPath = flags.String("kubeconfig", defaultKubeconfigPath(), "kubeconfig path to update")
		statePath      = flags.String("state", "", "additional state file path to remove")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if contextName == "" && flags.NArg() == 1 {
		contextName = flags.Arg(0)
	}
	if contextName == "" || flags.NArg() > 1 {
		return fmt.Errorf("usage: kubeconfig-proxy delete-context <context-name> [flags]")
	}

	absoluteKubeconfigPath, err := filepath.Abs(*kubeconfigPath)
	if err != nil {
		return err
	}
	statePaths, err := kubeconfig.DeleteProxyContext(absoluteKubeconfigPath, contextName)
	if err != nil {
		return err
	}
	if *statePath != "" {
		absoluteStatePath, err := filepath.Abs(*statePath)
		if err != nil {
			return err
		}
		statePaths = appendUniqueStrings(statePaths, absoluteStatePath)
	}
	if len(statePaths) == 0 {
		statePaths = append(statePaths, defaultStatePath(contextName))
	}
	if err := removeStateArtifacts(statePaths); err != nil {
		return err
	}

	log.Printf("updated kubeconfig: %s", absoluteKubeconfigPath)
	log.Printf("deleted context:    %q", contextName) // #nosec G706 -- %q escapes control characters in user-provided context names.
	for _, statePath := range statePaths {
		log.Printf("deleted state:      %q", statePath) // #nosec G706 -- %q escapes control characters in local state paths.
	}
	return nil
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
