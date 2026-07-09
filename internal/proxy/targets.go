package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Target struct {
	Name      string
	Host      *url.URL
	Namespace string
	Client    *http.Client
}

func LoadTargets(kubeconfigPath string, selectedContexts []string, primaryContext string) ([]Target, Target, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules = &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	}
	rawConfig, err := loadingRules.Load()
	if err != nil {
		return nil, Target{}, err
	}
	if len(rawConfig.Contexts) == 0 {
		return nil, Target{}, fmt.Errorf("source kubeconfig has no contexts")
	}

	contextNames, err := resolveContextNames(rawConfig, selectedContexts)
	if err != nil {
		return nil, Target{}, err
	}

	if primaryContext == "" {
		primaryContext = rawConfig.CurrentContext
	}
	if primaryContext != "" && !contains(contextNames, primaryContext) {
		return nil, Target{}, fmt.Errorf("primary context %q is not included in selected proxy contexts", primaryContext)
	}
	if primaryContext == "" {
		primaryContext = contextNames[0]
	}

	targets := make([]Target, 0, len(contextNames))
	for _, contextName := range contextNames {
		overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
		clientConfig := clientcmd.NewNonInteractiveClientConfig(*rawConfig, contextName, overrides, loadingRules)
		restConfig, err := clientConfig.ClientConfig()
		if err != nil {
			return nil, Target{}, fmt.Errorf("build client for context %q: %w", contextName, err)
		}
		target, err := targetFromRESTConfig(contextName, rawConfig.Contexts[contextName], restConfig)
		if err != nil {
			return nil, Target{}, err
		}
		targets = append(targets, target)
	}

	primaryIndex := 0
	for i, target := range targets {
		if target.Name == primaryContext {
			primaryIndex = i
			break
		}
	}

	return targets, targets[primaryIndex], nil
}

func TargetNames(targets []Target) string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	return strings.Join(names, ", ")
}

func resolveContextNames(rawConfig *clientcmdapi.Config, selectedContexts []string) ([]string, error) {
	if len(selectedContexts) > 0 {
		for _, contextName := range selectedContexts {
			if _, ok := rawConfig.Contexts[contextName]; !ok {
				return nil, fmt.Errorf("context %q not found in source kubeconfig", contextName)
			}
		}
		return selectedContexts, nil
	}

	contextNames := make([]string, 0, len(rawConfig.Contexts))
	for contextName := range rawConfig.Contexts {
		contextNames = append(contextNames, contextName)
	}
	sort.Strings(contextNames)
	return contextNames, nil
}

func targetFromRESTConfig(name string, kubeContext *clientcmdapi.Context, config *rest.Config) (Target, error) {
	host, err := url.Parse(config.Host)
	if err != nil {
		return Target{}, fmt.Errorf("parse host for context %q: %w", name, err)
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		return Target{}, fmt.Errorf("build transport for context %q: %w", name, err)
	}
	return Target{
		Name:      name,
		Host:      host,
		Namespace: kubeContext.Namespace,
		Client:    &http.Client{Transport: transport},
	}, nil
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
