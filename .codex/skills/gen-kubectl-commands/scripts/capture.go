// Command capture runs an ordered kubectl corpus through a local recording proxy.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

const (
	kindNodeImage  = "kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
	commandTimeout = 3 * time.Minute
	streamTimeout  = 5 * time.Second
)

type commandEntry struct {
	Command  string            `json:"command"`
	Requests []capturedRequest `json:"requests"`
}

type capturedRequest struct {
	Method string              `json:"method"`
	URL    string              `json:"url"`
	Args   map[string][]string `json:"args"`
	Body   string              `json:"body"`
}

type recorder struct {
	mu       sync.Mutex
	entries  *[]commandEntry
	active   int
	upstream *url.URL
	client   *http.Client
}

func (r *recorder) begin(index int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = index
}

func (r *recorder) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = -1
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, incoming *http.Request) {
	body, err := io.ReadAll(incoming.Body)
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	_ = incoming.Body.Close()
	r.record(incoming, body)

	upstreamRequest, err := r.newUpstreamRequest(incoming, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	response, err := r.client.Do(upstreamRequest) // #nosec G704 -- recorder forwards only to its temporary kind API server.
	if err != nil {
		http.Error(w, fmt.Sprintf("forward request: %v", err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyHeader(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	copyResponseBody(w, response.Body)
}

func copyResponseBody(w http.ResponseWriter, body io.Reader) {
	buffer := make([]byte, 32<<10)
	for {
		count, err := body.Read(buffer)
		if count > 0 {
			if _, writeErr := w.Write(buffer[:count]); writeErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *recorder) record(incoming *http.Request, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active < 0 {
		return
	}

	recordedURL := *r.upstream
	recordedURL.Path = incoming.URL.Path
	recordedURL.RawPath = incoming.URL.RawPath
	recordedURL.RawQuery = ""
	args := make(map[string][]string, len(incoming.URL.Query()))
	for key, values := range incoming.URL.Query() {
		args[key] = append([]string(nil), values...)
	}
	(*r.entries)[r.active].Requests = append((*r.entries)[r.active].Requests, capturedRequest{
		Method: incoming.Method,
		URL:    recordedURL.String(),
		Args:   args,
		Body:   captureBody(body),
	})
}

func (r *recorder) newUpstreamRequest(incoming *http.Request, body []byte) (*http.Request, error) {
	upstreamURL := *r.upstream
	upstreamURL.Path = incoming.URL.Path
	upstreamURL.RawPath = incoming.URL.RawPath
	upstreamURL.RawQuery = incoming.URL.RawQuery

	request, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	request.Header = incoming.Header.Clone()
	request.Header.Del("Authorization")
	request.Host = r.upstream.Host
	return request, nil
}

func captureBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if utf8.Valid(body) && bytes.IndexByte(body, 0) == -1 {
		return string(body)
	}
	return "base64:" + base64.StdEncoding.EncodeToString(body)
}

func main() {
	flags := flag.NewFlagSet("capture", flag.ExitOnError)
	outputPath := flags.String("output", filepath.Join(".codex", "skills", "gen-kubectl-commands", "kubectl-commands.yaml"), "YAML command corpus to execute and update")
	kubectlPath := flags.String("kubectl", "kubectl", "kubectl executable")
	clientPath := flags.String("client", "kubectl", "client executable to capture")
	clientName := flags.String("client-name", "kubectl", "command name expected in the corpus")
	projectDir := flags.String("project-dir", ".", "working directory for captured commands")
	kindPath := flags.String("kind", "kind", "kind executable")
	if err := flags.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "parse flags:", err)
		os.Exit(2)
	}

	if err := capture(*outputPath, *kubectlPath, *clientPath, *clientName, *projectDir, *kindPath); err != nil {
		fmt.Fprintln(os.Stderr, "capture failed:", err)
		os.Exit(1)
	}
}

func capture(outputPath, kubectlPath, clientPath, clientName, projectDir, kindPath string) error {
	if err := requireExecutable(kubectlPath); err != nil {
		return err
	}
	if err := requireExecutable(clientPath); err != nil {
		return err
	}
	if err := requireExecutable(kindPath); err != nil {
		return err
	}

	absOutputPath, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	entries, err := loadEntries(absOutputPath, clientName)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("command corpus is empty")
	}
	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("resolve project directory: %w", err)
	}
	if info, err := os.Stat(absProjectDir); err != nil {
		return fmt.Errorf("stat project directory: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("project directory %q is not a directory", absProjectDir)
	}

	workDir, err := os.MkdirTemp("", "kcp-http-capture-")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	clusterName, err := temporaryClusterName()
	if err != nil {
		return err
	}
	sourceKubeconfig := filepath.Join(workDir, "kind.yaml")
	if err := run(context.Background(), kindPath, "create", "cluster", "--name", clusterName, "--image", kindNodeImage, "--kubeconfig", sourceKubeconfig, "--wait", "3m"); err != nil {
		return fmt.Errorf("create temporary kind cluster: %w", err)
	}
	defer func() {
		if err := run(context.Background(), kindPath, "delete", "cluster", "--name", clusterName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: delete temporary kind cluster %q: %v\n", clusterName, err)
		}
	}()
	if err := warmImage(kubectlPath, sourceKubeconfig); err != nil {
		return err
	}

	restConfig, rawConfig, contextName, err := loadKubeconfig(sourceKubeconfig)
	if err != nil {
		return err
	}
	transport, err := rest.TransportFor(restConfig)
	if err != nil {
		return fmt.Errorf("create upstream transport: %w", err)
	}
	upstream, err := url.Parse(restConfig.Host)
	if err != nil {
		return fmt.Errorf("parse kind API server URL: %w", err)
	}
	if upstream.Scheme != "https" || upstream.Host == "" {
		return fmt.Errorf("kind API server URL must be HTTPS, got %q", restConfig.Host)
	}

	certificate, caPEM, err := issueRecorderCertificate()
	if err != nil {
		return err
	}
	recorder := &recorder{active: -1, entries: &entries, upstream: upstream, client: &http.Client{Transport: transport}}
	server, recorderURL, err := startRecorder(recorder, certificate)
	if err != nil {
		return err
	}
	defer server.Close()

	captureKubeconfig := filepath.Join(workDir, "capture.yaml")
	if err := writeCaptureKubeconfig(rawConfig, contextName, recorderURL, caPEM, captureKubeconfig); err != nil {
		return err
	}

	for index := range entries {
		entries[index].Requests = nil
		recorder.begin(index)
		err := runClient(clientPath, clientName, captureKubeconfig, workDir, absProjectDir, entries[index].Command)
		recorder.end()
		if writeErr := writeEntries(absOutputPath, entries); writeErr != nil {
			return writeErr
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "command %d failed after capture: %s: %v\n", index+1, entries[index].Command, err)
		}
	}

	return nil
}

func requireExecutable(name string) error {
	if strings.ContainsRune(name, filepath.Separator) {
		if _, err := os.Stat(name); err != nil {
			return fmt.Errorf("required executable %q: %w", name, err)
		}
		return nil
	}
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("required executable %q not found", name)
	}
	return nil
}

func loadEntries(path, clientName string) ([]commandEntry, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- caller explicitly selects the disposable corpus path.
	if err != nil {
		return nil, fmt.Errorf("read command corpus: %w", err)
	}
	var entries []commandEntry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse command corpus: %w", err)
	}
	for index, entry := range entries {
		if !strings.HasPrefix(entry.Command, clientName+" ") {
			return nil, fmt.Errorf("entry %d command must start with %q", index+1, clientName+" ")
		}
	}
	return entries, nil
}

func writeEntries(path string, entries []commandEntry) error {
	for index := range entries {
		if entries[index].Requests == nil {
			entries[index].Requests = []capturedRequest{}
		}
	}
	data, err := yaml.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode captured corpus: %w", err)
	}
	temporaryFile, err := os.CreateTemp(filepath.Dir(path), ".kubectl-commands-*")
	if err != nil {
		return fmt.Errorf("create temporary captured corpus: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	defer os.Remove(temporaryPath)
	if err := temporaryFile.Chmod(0o600); err != nil {
		_ = temporaryFile.Close()
		return fmt.Errorf("set temporary captured corpus permissions: %w", err)
	}
	if _, err := temporaryFile.Write(data); err != nil {
		_ = temporaryFile.Close()
		return fmt.Errorf("write temporary captured corpus: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return fmt.Errorf("close temporary captured corpus: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace captured corpus: %w", err)
	}
	return nil
}

func temporaryClusterName() (string, error) {
	random := make([]byte, 5)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create cluster suffix: %w", err)
	}
	return "kcp-http-capture-" + hex.EncodeToString(random), nil
}

func warmImage(kubectlPath, kubeconfigPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := run(ctx, kubectlPath, "--kubeconfig", kubeconfigPath, "run", "kcp-http-image-warmup", "--image=busybox:1.37.0", "--restart=Never", "--command", "--", "sh", "-c", "sleep 600"); err != nil {
		return fmt.Errorf("create image warm-up pod: %w", err)
	}
	if err := run(ctx, kubectlPath, "--kubeconfig", kubeconfigPath, "wait", "--for=condition=Ready", "pod/kcp-http-image-warmup", "--timeout=2m"); err != nil {
		return fmt.Errorf("wait for image warm-up pod: %w", err)
	}
	return nil
}

func loadKubeconfig(path string) (*rest.Config, clientcmdapi.Config, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = path
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, clientcmdapi.Config{}, "", fmt.Errorf("load kind REST config: %w", err)
	}
	rawConfig, err := clientConfig.RawConfig()
	if err != nil {
		return nil, clientcmdapi.Config{}, "", fmt.Errorf("load kind kubeconfig: %w", err)
	}
	if rawConfig.CurrentContext == "" {
		return nil, clientcmdapi.Config{}, "", errors.New("kind kubeconfig has no current context")
	}
	return restConfig, rawConfig, rawConfig.CurrentContext, nil
}

func issueRecorderCertificate() (tls.Certificate, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate recorder key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate recorder certificate serial: %w", err)
	}
	certificateTemplate := x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: "kcp-http-capture"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, &certificateTemplate, &certificateTemplate, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("sign recorder certificate: %w", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load recorder certificate: %w", err)
	}
	return certificate, certificatePEM, nil
}

func startRecorder(handler http.Handler, certificate tls.Certificate) (*http.Server, string, error) {
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{ // #nosec G402 -- the ephemeral recorder needs only TLS 1.2+ defaults.
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, "", fmt.Errorf("listen for recording proxy: %w", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "recording proxy failed:", err)
		}
	}()
	return server, "https://" + listener.Addr().String(), nil
}

func writeCaptureKubeconfig(config clientcmdapi.Config, contextName, recorderURL string, caPEM []byte, path string) error {
	contextConfig, ok := config.Contexts[contextName]
	if !ok {
		return fmt.Errorf("current context %q is absent from kind kubeconfig", contextName)
	}
	cluster, ok := config.Clusters[contextConfig.Cluster]
	if !ok {
		return fmt.Errorf("cluster %q is absent from kind kubeconfig", contextConfig.Cluster)
	}
	cluster.Server = recorderURL
	cluster.CertificateAuthority = ""
	cluster.CertificateAuthorityData = caPEM
	cluster.InsecureSkipTLSVerify = false
	if err := clientcmd.WriteToFile(config, path); err != nil {
		return fmt.Errorf("write recording kubeconfig: %w", err)
	}
	return nil
}

func runClient(clientPath, clientName, kubeconfigPath, temporaryDirectory, projectDir, command string) error {
	timeout := commandTimeout
	if isStreamCommand(command) {
		timeout = streamTimeout
	}
	context, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	process := exec.CommandContext(context, "/bin/sh", "-c", shellQuote(clientPath)+strings.TrimPrefix(command, clientName)) // #nosec G204 -- commands are the reviewed disposable-cluster corpus.
	process.Dir = projectDir
	process.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath, "TMPDIR="+temporaryDirectory)
	if clientName == "werf" {
		process.Env = append(process.Env, "WERF_HOME="+filepath.Join(temporaryDirectory, "werf-home"))
	}
	output, err := process.CombinedOutput()
	if context.Err() != nil {
		return fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func isStreamCommand(command string) bool {
	for _, name := range []string{" attach ", " port-forward ", " proxy ", " logs --follow", " logs -f", "werf dismiss "} {
		if strings.Contains(" "+command, name) {
			return true
		}
	}
	return false
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\\"'\\\"'") + "'"
}

func run(ctx context.Context, executable string, args ...string) error {
	process := exec.CommandContext(ctx, executable, args...) // #nosec G204 -- executable and arguments are controlled collector inputs.
	output, err := process.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func copyHeader(destination, source http.Header) {
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}
