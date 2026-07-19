package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
	"k8s.io/client-go/tools/clientcmd"
)

func runAddContext(args []string) error {
	flags := flag.NewFlagSet("kubeconfig-proxy add-context", flag.ContinueOnError)
	contextName := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		contextName = args[0]
		args = args[1:]
	}
	var (
		kubeconfigPath = flags.String("kubeconfig", defaultKubeconfigPath(), "kubeconfig path to update")
		statePath      = flags.String("state", "", "state file path; defaults to ~/.kube/kubeconfig-proxy/<context>.yaml")
		listenAddr     = flags.String("listen", "", "proxy listen address; defaults to an available 127.0.0.1 port")
		contextsCSV    = flags.String("contexts", "", "comma-separated source kubeconfig contexts to include")
		contextRegexp  = flags.String("context-regexp", "", "regular expression for source context names")
		primaryContext = flags.String("primary-context", "", "context used for single-cluster operations")
		proxyTTL       = flags.Duration("proxy-ttl", 10*time.Minute, "time after the last active request before the proxy exits; 0 disables it")
		requestTimeout = flags.Duration("request-timeout", 30*time.Second, "timeout for one upstream Kubernetes API request; 0 disables it")
		retries        = flags.Int("retries", proxy.DefaultRetries, "number of retries for failed upstream requests")
		retryBackoff   = flags.Duration("retry-backoff", 200*time.Millisecond, "delay between upstream request retries")
		helmRelease    = flags.Bool("helm-release-proxy", false, "proxy Helm release storage list/watch requests only through the primary context")
		readOnly       = flags.Bool("read-only", false, "reject mutating Kubernetes API requests with 403")
		logsEnabled    = flags.Bool("logs-enabled", false, "write serve logs to the state log file")
		execCommand    = flags.String("exec-command", defaultExecCommand(), "command written to kubeconfig exec auth")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if contextName == "" && flags.NArg() == 1 {
		contextName = flags.Arg(0)
	}
	if contextName == "" || flags.NArg() > 1 {
		return fmt.Errorf("usage: kubeconfig-proxy add-context <context-name> [flags]")
	}

	absoluteKubeconfigPath, err := filepath.Abs(*kubeconfigPath)
	if err != nil {
		return err
	}
	if *statePath == "" {
		*statePath = defaultStatePath(contextName)
	}
	absoluteStatePath, err := filepath.Abs(*statePath)
	if err != nil {
		return err
	}
	resolvedListenAddr, err := resolveAddContextListenAddr(*listenAddr)
	if err != nil {
		return err
	}

	selectedContexts, selectedPrimary, err := resolveAddContextTargets(absoluteKubeconfigPath, contextName, splitCSV(*contextsCSV), *contextRegexp, *primaryContext)
	if err != nil {
		return err
	}
	targets, primary, err := proxy.LoadTargets(absoluteKubeconfigPath, selectedContexts, selectedPrimary)
	if err != nil {
		return err
	}

	bearerToken, err := generateBearerToken()
	if err != nil {
		return err
	}
	_, certPEM, keyPEM, err := generateTLSCertificate(resolvedListenAddr)
	if err != nil {
		return err
	}

	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             contextName,
		SourceKubeconfig: absoluteKubeconfigPath,
		Listen:           resolvedListenAddr,
		Contexts:         targetNames(targets),
		PrimaryContext:   primary.Name,
		BearerToken:      bearerToken,
		ProxyTTL:         proxyTTL.String(),
		LogsEnabled:      *logsEnabled,
		TLS: proxystate.TLS{
			CertPEM: string(certPEM),
			KeyPEM:  string(keyPEM),
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout:   requestTimeout.String(),
			Retries:          *retries,
			RetryBackoff:     retryBackoff.String(),
			HelmReleaseProxy: *helmRelease,
			ReadOnly:         *readOnly,
		},
	}
	if err := proxystate.Save(absoluteStatePath, profile); err != nil {
		return err
	}

	serverURL := "https://" + resolvedListenAddr
	if err := kubeconfig.AddProxyContext(absoluteKubeconfigPath, contextName, serverURL, primary.Namespace, *execCommand, absoluteStatePath, certPEM); err != nil {
		return err
	}

	log.Printf("updated kubeconfig: %s", absoluteKubeconfigPath)
	log.Printf("state file:         %s", absoluteStatePath)
	log.Printf("context:            %q", contextName) // #nosec G706 -- %q escapes control characters in user-provided context names.
	log.Printf("listen:             %s", serverURL)
	log.Printf("targets:            %s", proxy.TargetNames(targets))
	log.Printf("primary target:     %s", primary.Name)
	log.Printf("proxy ttl:          %s", durationLogValue(*proxyTTL))
	log.Printf("read only:          %t", *readOnly)
	log.Printf("serve logs:         %t", *logsEnabled)
	return nil
}

func resolveAddContextTargets(kubeconfigPath, proxyContextName string, selectedContexts []string, contextRegexp, primaryContext string) ([]string, string, error) {
	if len(selectedContexts) > 0 && strings.TrimSpace(contextRegexp) != "" {
		return nil, "", fmt.Errorf("--contexts and --context-regexp are mutually exclusive")
	}

	rawConfig, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, "", err
	}

	var contextNames []string
	switch {
	case len(selectedContexts) > 0:
		contextNames = append([]string(nil), selectedContexts...)
	case strings.TrimSpace(contextRegexp) != "":
		re, err := regexp.Compile(contextRegexp)
		if err != nil {
			return nil, "", err
		}
		for name := range rawConfig.Contexts {
			if name != proxyContextName && re.MatchString(name) {
				contextNames = append(contextNames, name)
			}
		}
		sort.Strings(contextNames)
	default:
		for name := range rawConfig.Contexts {
			if name != proxyContextName {
				contextNames = append(contextNames, name)
			}
		}
		sort.Strings(contextNames)
	}
	if len(contextNames) == 0 {
		return nil, "", fmt.Errorf("no source contexts selected")
	}
	for _, name := range contextNames {
		if name == proxyContextName {
			return nil, "", fmt.Errorf("source contexts must not include proxy context %q", proxyContextName)
		}
		if _, ok := rawConfig.Contexts[name]; !ok {
			return nil, "", fmt.Errorf("context %q not found in source kubeconfig", name)
		}
	}

	if primaryContext == "" {
		if contains(contextNames, rawConfig.CurrentContext) {
			primaryContext = rawConfig.CurrentContext
		} else {
			primaryContext = contextNames[0]
		}
	}
	if !contains(contextNames, primaryContext) {
		return nil, "", fmt.Errorf("primary context %q is not included in selected proxy contexts", primaryContext)
	}
	return contextNames, primaryContext, nil
}
