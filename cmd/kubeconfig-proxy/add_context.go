package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/upstream"
)

type addContextOptions struct {
	contextName    string
	kubeconfigPath string
	statePath      string
	listenAddr     string
	contextsCSV    string
	contextRegexp  string
	primaryContext string
	proxyTTL       time.Duration
	requestTimeout time.Duration
	retries        int
	retryBackoff   time.Duration
	helmRelease    bool
	readOnly       bool
	logsEnabled    bool
	execCommand    string
}

func runAddContext(args []string) error {
	options, err := parseAddContextOptions(args)
	if err != nil {
		return err
	}

	absoluteKubeconfigPath, absoluteStatePath, err := resolveAddContextPaths(options.kubeconfigPath, options.statePath, options.contextName)
	if err != nil {
		return err
	}
	resolvedListenAddr, err := resolveAddContextListenAddr(options.listenAddr)
	if err != nil {
		return err
	}

	source, err := kubeconfig.LoadSource(absoluteKubeconfigPath)
	if err != nil {
		return err
	}
	selectedContexts, selectedPrimary, err := source.SelectContexts(kubeconfig.ContextSelection{
		ProxyContextName: options.contextName,
		SelectedContexts: splitCSV(options.contextsCSV),
		ContextRegexp:    options.contextRegexp,
		PrimaryContext:   options.primaryContext,
	})
	if err != nil {
		return err
	}
	targets, primary, err := upstream.LoadTargets(source, selectedContexts, selectedPrimary)
	if err != nil {
		return err
	}

	profile, certPEM, err := newAddContextProfile(options, absoluteKubeconfigPath, resolvedListenAddr, targets, primary)
	if err != nil {
		return err
	}
	if err := proxystate.Save(absoluteStatePath, profile); err != nil {
		return err
	}

	serverURL := "https://" + resolvedListenAddr
	if err := kubeconfig.AddProxyContext(absoluteKubeconfigPath, options.contextName, serverURL, primary.Namespace, options.execCommand, absoluteStatePath, certPEM); err != nil {
		return err
	}

	logAddContextResult(options, absoluteKubeconfigPath, absoluteStatePath, serverURL, targets, primary)
	return nil
}

func parseAddContextOptions(args []string) (addContextOptions, error) {
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
		return addContextOptions{}, err
	}
	contextName, err := resolveAddContextName(contextName, flags)
	if err != nil {
		return addContextOptions{}, err
	}
	return addContextOptions{
		contextName:    contextName,
		kubeconfigPath: *kubeconfigPath,
		statePath:      *statePath,
		listenAddr:     *listenAddr,
		contextsCSV:    *contextsCSV,
		contextRegexp:  *contextRegexp,
		primaryContext: *primaryContext,
		proxyTTL:       *proxyTTL,
		requestTimeout: *requestTimeout,
		retries:        *retries,
		retryBackoff:   *retryBackoff,
		helmRelease:    *helmRelease,
		readOnly:       *readOnly,
		logsEnabled:    *logsEnabled,
		execCommand:    *execCommand,
	}, nil
}

func resolveAddContextName(contextName string, flags *flag.FlagSet) (string, error) {
	if contextName != "" && flags.NArg() > 0 {
		return "", fmt.Errorf("usage: kubeconfig-proxy add-context <context-name> [flags]")
	}
	if contextName == "" && flags.NArg() == 1 {
		contextName = flags.Arg(0)
	}
	if contextName == "" {
		return "", fmt.Errorf("usage: kubeconfig-proxy add-context <context-name> [flags]")
	}
	return contextName, nil
}

func resolveAddContextPaths(kubeconfigPath, statePath, contextName string) (string, string, error) {
	absoluteKubeconfigPath, err := filepath.Abs(kubeconfigPath)
	if err != nil {
		return "", "", err
	}
	if statePath == "" {
		statePath, err = defaultStatePath(contextName)
		if err != nil {
			return "", "", err
		}
	}
	absoluteStatePath, err := filepath.Abs(statePath)
	if err != nil {
		return "", "", err
	}
	return absoluteKubeconfigPath, absoluteStatePath, nil
}

func newAddContextProfile(options addContextOptions, kubeconfigPath, listenAddr string, targets []upstream.Target, primary upstream.Target) (*proxystate.Profile, []byte, error) {
	bearerToken, err := generateBearerToken()
	if err != nil {
		return nil, nil, err
	}
	certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		return nil, nil, err
	}

	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             options.contextName,
		SourceKubeconfig: kubeconfigPath,
		Listen:           listenAddr,
		Contexts:         upstream.NameList(targets),
		PrimaryContext:   primary.Name,
		BearerToken:      bearerToken,
		ProxyTTL:         options.proxyTTL.String(),
		LogsEnabled:      options.logsEnabled,
		TLS: proxystate.TLS{
			CertPEM: string(certPEM),
			KeyPEM:  string(keyPEM),
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout:   options.requestTimeout.String(),
			Retries:          options.retries,
			RetryBackoff:     options.retryBackoff.String(),
			HelmReleaseProxy: options.helmRelease,
			ReadOnly:         options.readOnly,
		},
	}
	return profile, certPEM, nil
}

func logAddContextResult(options addContextOptions, kubeconfigPath, statePath, serverURL string, targets []upstream.Target, primary upstream.Target) {
	log.Printf("updated kubeconfig: %s", kubeconfigPath)
	log.Printf("state file:         %s", statePath)
	log.Printf("context:            %q", options.contextName) // #nosec G706 -- %q escapes control characters in user-provided context names.
	log.Printf("listen:             %s", serverURL)
	log.Printf("targets:            %s", upstream.Names(targets))
	log.Printf("primary target:     %s", primary.Name)
	log.Printf("proxy ttl:          %s", durationLogValue(options.proxyTTL))
	log.Printf("read only:          %t", options.readOnly)
	log.Printf("serve logs:         %t", options.logsEnabled)
}
