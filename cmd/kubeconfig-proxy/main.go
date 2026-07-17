package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
	"k8s.io/client-go/tools/clientcmd"
)

const readinessPath = "/-/kubeconfig-proxy/ready"

func main() {
	if err := run(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run() error {
	return runWithArgs(os.Args[1:], nil)
}

func runWithArgs(args []string, stop <-chan os.Signal) error {
	if len(args) > 0 {
		switch args[0] {
		case "add-context":
			return runAddContext(args[1:])
		case "delete-context":
			return runDeleteContext(args[1:])
		case "credential":
			return runCredential(args[1:])
		case "serve":
			return runServeState(args[1:], stop)
		}
	}
	return fmt.Errorf("usage: kubeconfig-proxy <add-context|delete-context|credential|serve> [flags]")
}

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
	log.Printf("context:            %s", contextName)
	log.Printf("listen:             %s", serverURL)
	log.Printf("targets:            %s", proxy.TargetNames(targets))
	log.Printf("primary target:     %s", primary.Name)
	log.Printf("proxy ttl:          %s", durationLogValue(*proxyTTL))
	log.Printf("serve logs:         %t", *logsEnabled)
	return nil
}

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
	log.Printf("deleted context:    %s", contextName)
	for _, statePath := range statePaths {
		log.Printf("deleted state:      %s", statePath)
	}
	return nil
}

func runCredential(args []string) error {
	flags := flag.NewFlagSet("kubeconfig-proxy credential", flag.ContinueOnError)
	statePath := flags.String("state", "", "state file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *statePath == "" {
		return fmt.Errorf("--state is required")
	}

	unlock, err := lockState(*statePath)
	if err != nil {
		return err
	}
	defer unlock()

	profile, err := proxystate.Load(*statePath)
	if err != nil {
		return err
	}
	proxyTTL, err := profile.ProxyTTLDuration()
	if err != nil {
		return err
	}
	if !ready(profile) {
		if err := startDetachedServe(*statePath, profile.LogsEnabled); err != nil {
			return err
		}
		if err := waitReady(profile, 10*time.Second); err != nil {
			return err
		}
	}

	return writeExecCredential(os.Stdout, profile.BearerToken, execCredentialExpiration(time.Now(), proxyTTL))
}

func runServeState(args []string, stop <-chan os.Signal) error {
	flags := flag.NewFlagSet("kubeconfig-proxy serve", flag.ContinueOnError)
	statePath := flags.String("state", "", "state file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *statePath == "" {
		return fmt.Errorf("--state is required")
	}

	profile, err := proxystate.Load(*statePath)
	if err != nil {
		return err
	}
	if !profile.LogsEnabled {
		restoreLogs := discardLogs()
		defer restoreLogs()
	}
	requestTimeout, err := profile.RequestTimeoutDuration()
	if err != nil {
		return err
	}
	retryBackoff, err := profile.RetryBackoffDuration()
	if err != nil {
		return err
	}
	proxyTTL, err := profile.ProxyTTLDuration()
	if err != nil {
		return err
	}

	targets, primary, err := proxy.LoadTargets(profile.SourceKubeconfig, profile.Contexts, profile.PrimaryContext)
	if err != nil {
		return err
	}
	handler, err := proxy.NewWithOptions(targets, primary, proxy.Options{
		RequestTimeout:   requestTimeout,
		Retries:          profile.Options.Retries,
		RetryBackoff:     retryBackoff,
		BearerToken:      profile.BearerToken,
		HelmReleaseProxy: profile.Options.HelmReleaseProxy,
	})
	if err != nil {
		return err
	}

	tlsCertificate, err := tls.X509KeyPair([]byte(profile.TLS.CertPEM), []byte(profile.TLS.KeyPEM))
	if err != nil {
		return fmt.Errorf("load TLS key pair from state: %w", err)
	}
	listener, err := proxy.Listen(profile.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	log.Printf("state file:       %s", *statePath)
	log.Printf("listen:           https://%s", listener.Addr().String())
	log.Printf("targets:          %s", proxy.TargetNames(targets))
	log.Printf("primary target:   %s", primary.Name)
	log.Printf("proxy ttl:        %s", durationLogValue(proxyTTL))
	log.Printf("request timeout: %s", durationLogValue(requestTimeout))
	log.Printf("retries:         %d", profile.Options.Retries)
	log.Printf("retry backoff:   %s", retryBackoff)

	return serveHTTP(listener, handler, tlsCertificate, proxyTTL, profile.BearerToken, stop)
}

func serveHTTP(listener net.Listener, handler http.Handler, tlsCertificate tls.Certificate, proxyTTL time.Duration, bearerToken string, stop <-chan os.Signal) error {
	activityHandler := newActivityHandler(handler, bearerToken)
	server := &http.Server{Addr: listener.Addr().String(), Handler: activityHandler}
	errCh := make(chan error, 1)
	go func() {
		tlsListener := tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{tlsCertificate},
			MinVersion:   tls.VersionTLS12,
		})
		errCh <- server.Serve(tlsListener)
	}()

	ttlCh := (<-chan time.Time)(nil)
	ticker := (*time.Ticker)(nil)
	if proxyTTL > 0 {
		ticker = time.NewTicker(ttlCheckInterval(proxyTTL))
		defer ticker.Stop()
		ttlCh = ticker.C
	}

	if stop == nil {
		signalStop := make(chan os.Signal, 1)
		signal.Notify(signalStop, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signalStop)
		stop = signalStop
	}

	for {
		select {
		case <-stop:
			log.Printf("shutting down")
			return shutdownServer(server)
		case <-ttlCh:
			if activityHandler.idleFor(proxyTTL) {
				log.Printf("shutting down after %s without active requests", proxyTTL)
				return shutdownServer(server)
			}
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	}
}

type activityHandler struct {
	next         http.Handler
	bearerToken  string
	lastActivity atomic.Int64
	inFlight     atomic.Int64
}

func newActivityHandler(next http.Handler, bearerToken string) *activityHandler {
	h := &activityHandler{next: next, bearerToken: bearerToken}
	h.lastActivity.Store(time.Now().UnixNano())
	return h
}

func (h *activityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == readinessPath {
		if !authorizedWithToken(r, h.bearerToken) {
			writePlainStatus(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	h.inFlight.Add(1)
	h.lastActivity.Store(time.Now().UnixNano())
	defer func() {
		h.lastActivity.Store(time.Now().UnixNano())
		h.inFlight.Add(-1)
	}()
	h.next.ServeHTTP(w, r)
}

func (h *activityHandler) idleFor(ttl time.Duration) bool {
	if h.inFlight.Load() > 0 {
		return false
	}
	return time.Since(time.Unix(0, h.lastActivity.Load())) > ttl
}

func authorizedWithToken(r *http.Request, token string) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writePlainStatus(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}

func shutdownServer(server *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), proxy.ShutdownTimeout)
	defer cancel()
	return server.Shutdown(ctx)
}

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

func resolveAddContextListenAddr(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return pickAvailableListenAddr()
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("parse listen address: %w", err)
	}
	if port == "0" {
		return pickAvailableListenAddrForHost(host)
	}
	return value, nil
}

func pickAvailableListenAddr() (string, error) {
	return pickAvailableListenAddrForHost("127.0.0.1")
}

func pickAvailableListenAddrForHost(host string) (string, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}

func generateBearerToken() (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("generate proxy bearer token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func generateTLSCertificate(addr string) (tls.Certificate, []byte, []byte, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("parse listen address for TLS certificate: %w", err)
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("generate TLS private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("generate TLS serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "kubeconfig-proxy",
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	} else if host != "" {
		template.DNSNames = append(template.DNSNames, host)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
		template.DNSNames = append(template.DNSNames, "localhost")
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("create TLS certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("marshal TLS private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return certificate, certPEM, keyPEM, nil
}

func lockState(statePath string) (func(), error) {
	lockPath := statePath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func ready(profile *proxystate.Profile) bool {
	return checkReady(profile) == nil
}

func waitReady(profile *proxystate.Profile, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := checkReady(profile); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("proxy did not become ready at https://%s: %w", profile.Listen, lastErr)
}

func checkReady(profile *proxystate.Profile) error {
	client, err := profileHTTPClient(profile)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, "https://"+profile.Listen+readinessPath, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+profile.BearerToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness status %d", resp.StatusCode)
	}
	return nil
}

func profileHTTPClient(profile *proxystate.Profile) (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(profile.TLS.CertPEM)) {
		return nil, fmt.Errorf("state TLS certificate is not valid PEM")
	}
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: roots,
		}},
	}, nil
}

func startDetachedServe(statePath string, logsEnabled bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	nullFile, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer nullFile.Close()

	stdout := io.Writer(nullFile)
	stderr := io.Writer(nullFile)
	if logsEnabled {
		logPath := statePath + ".log"
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer logFile.Close()
		stdout = logFile
		stderr = logFile
	}

	cmd := exec.Command(executable, "serve", "--state", statePath)
	cmd.Stdin = nullFile
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}

func discardLogs() func() {
	output := log.Writer()
	flags := log.Flags()
	prefix := log.Prefix()
	log.SetOutput(io.Discard)
	return func() {
		log.SetOutput(output)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}
}

func removeStateArtifacts(statePaths []string) error {
	for _, statePath := range statePaths {
		for _, path := range []string{statePath, statePath + ".log", statePath + ".lock"} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func writeExecCredential(w io.Writer, token string, expiration *time.Time) error {
	status := map[string]any{
		"token": token,
	}
	if expiration != nil {
		status["expirationTimestamp"] = expiration.Format(time.RFC3339)
	}
	credential := map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1",
		"kind":       "ExecCredential",
		"status":     status,
	}
	encoder := json.NewEncoder(w)
	return encoder.Encode(credential)
}

func execCredentialExpiration(now time.Time, proxyTTL time.Duration) *time.Time {
	if proxyTTL <= 0 {
		return nil
	}
	skew := proxyTTL / 10
	if skew > 10*time.Second {
		skew = 10 * time.Second
	}
	if skew <= 0 {
		skew = proxyTTL / 2
	}
	validFor := proxyTTL - skew
	if validFor <= 0 {
		validFor = proxyTTL / 2
	}
	expiration := now.Add(validFor).UTC()
	return &expiration
}

func ttlCheckInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return time.Second
	}
	interval := ttl / 4
	if interval < 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	if interval > time.Second {
		return time.Second
	}
	return interval
}
