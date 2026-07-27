package upstream

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc" // register legacy kubeconfig OIDC auth-provider
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Target struct {
	Name      string
	Host      *url.URL
	Namespace string
	Client    *http.Client
}

func LoadTargets(source *kubeconfig.Source, selectedContexts []string, primaryContext string) ([]Target, Target, error) {
	if source == nil {
		return nil, Target{}, fmt.Errorf("source kubeconfig is required")
	}
	contextNames, err := source.ResolveContexts(selectedContexts)
	if err != nil {
		return nil, Target{}, err
	}

	if primaryContext == "" {
		primaryContext = source.CurrentContext()
	}
	if primaryContext != "" && !slices.Contains(contextNames, primaryContext) {
		return nil, Target{}, fmt.Errorf("primary context %q is not included in selected proxy contexts", primaryContext)
	}
	if primaryContext == "" {
		primaryContext = contextNames[0]
	}

	targets := make([]Target, 0, len(contextNames))
	for _, contextName := range contextNames {
		kubeContext, restConfig, err := source.ClientConfig(contextName)
		if err != nil {
			return nil, Target{}, err
		}
		target, err := targetFromRESTConfig(contextName, kubeContext, restConfig)
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

func Names(targets []Target) string {
	return strings.Join(NameList(targets), ", ")
}

func NameList(targets []Target) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	return names
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
