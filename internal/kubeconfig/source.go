package kubeconfig

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Source struct {
	rawConfig    *clientcmdapi.Config
	loadingRules *clientcmd.ClientConfigLoadingRules
}

type ContextSelection struct {
	ProxyContextName string
	SelectedContexts []string
	ContextRegexp    string
	PrimaryContext   string
}

func LoadSource(path string) (*Source, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		loadingRules = &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
	}
	rawConfig, err := loadingRules.Load()
	if err != nil {
		return nil, err
	}
	if len(rawConfig.Contexts) == 0 {
		return nil, fmt.Errorf("source kubeconfig has no contexts")
	}
	return &Source{rawConfig: rawConfig, loadingRules: loadingRules}, nil
}

func (s *Source) SelectContexts(selection ContextSelection) ([]string, string, error) {
	contextNames, err := s.selectContextNames(selection)
	if err != nil {
		return nil, "", err
	}
	if err := s.validateSelectedContexts(selection.ProxyContextName, contextNames); err != nil {
		return nil, "", err
	}

	primaryContext := selection.PrimaryContext
	if primaryContext == "" {
		if slices.Contains(contextNames, s.rawConfig.CurrentContext) {
			primaryContext = s.rawConfig.CurrentContext
		} else {
			primaryContext = contextNames[0]
		}
	}
	if !slices.Contains(contextNames, primaryContext) {
		return nil, "", fmt.Errorf("primary context %q is not included in selected proxy contexts", primaryContext)
	}
	return contextNames, primaryContext, nil
}

func (s *Source) ResolveContexts(selectedContexts []string) ([]string, error) {
	if len(selectedContexts) > 0 {
		contextNames := append([]string(nil), selectedContexts...)
		if err := s.validateContextNames(contextNames); err != nil {
			return nil, err
		}
		return contextNames, nil
	}

	contextNames := make([]string, 0, len(s.rawConfig.Contexts))
	for contextName := range s.rawConfig.Contexts {
		contextNames = append(contextNames, contextName)
	}
	sort.Strings(contextNames)
	return contextNames, nil
}

func (s *Source) CurrentContext() string {
	return s.rawConfig.CurrentContext
}

func (s *Source) ClientConfig(contextName string) (*clientcmdapi.Context, *rest.Config, error) {
	kubeContext, ok := s.rawConfig.Contexts[contextName]
	if !ok || kubeContext == nil {
		return nil, nil, fmt.Errorf("context %q not found in source kubeconfig", contextName)
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	clientConfig := clientcmd.NewNonInteractiveClientConfig(*s.rawConfig, contextName, overrides, s.loadingRules)
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("build client for context %q: %w", contextName, err)
	}
	return kubeContext, restConfig, nil
}

func (s *Source) selectContextNames(selection ContextSelection) ([]string, error) {
	contextNames := append([]string(nil), selection.SelectedContexts...)
	selectedNames := make(map[string]struct{}, len(contextNames))
	for _, name := range contextNames {
		selectedNames[name] = struct{}{}
	}

	if strings.TrimSpace(selection.ContextRegexp) != "" {
		re, err := regexp.Compile(selection.ContextRegexp)
		if err != nil {
			return nil, err
		}
		matchedNames := make([]string, 0, len(s.rawConfig.Contexts))
		for name := range s.rawConfig.Contexts {
			if !s.isManagedProxyContext(name) && re.MatchString(name) {
				matchedNames = append(matchedNames, name)
			}
		}
		sort.Strings(matchedNames)
		for _, name := range matchedNames {
			if _, ok := selectedNames[name]; ok {
				continue
			}
			contextNames = append(contextNames, name)
			selectedNames[name] = struct{}{}
		}
	}

	if len(selection.SelectedContexts) == 0 && strings.TrimSpace(selection.ContextRegexp) == "" {
		for name := range s.rawConfig.Contexts {
			if !s.isManagedProxyContext(name) {
				contextNames = append(contextNames, name)
			}
		}
		sort.Strings(contextNames)
	}
	return contextNames, nil
}

func (s *Source) isManagedProxyContext(contextName string) bool {
	kubeContext := s.rawConfig.Contexts[contextName]
	if kubeContext == nil {
		return false
	}
	entryName := formatProxyEntryName(contextName)
	return kubeContext.Cluster == entryName && kubeContext.AuthInfo == entryName
}

func (s *Source) validateSelectedContexts(proxyContextName string, contextNames []string) error {
	if len(contextNames) == 0 {
		return fmt.Errorf("no source contexts selected")
	}
	for _, name := range contextNames {
		if name == proxyContextName {
			return fmt.Errorf("source contexts must not include proxy context %q", proxyContextName)
		}
	}
	return s.validateContextNames(contextNames)
}

func (s *Source) validateContextNames(contextNames []string) error {
	seen := make(map[string]struct{}, len(contextNames))
	for _, name := range contextNames {
		if _, ok := seen[name]; ok {
			return fmt.Errorf("context %q is selected more than once", name)
		}
		seen[name] = struct{}{}
		if context, ok := s.rawConfig.Contexts[name]; !ok || context == nil {
			return fmt.Errorf("context %q not found in source kubeconfig", name)
		}
	}
	return nil
}
