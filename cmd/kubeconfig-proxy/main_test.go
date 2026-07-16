package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestRunWithArgsRequiresSubcommand(t *testing.T) {
	err := runWithArgs(nil, nil)
	if err == nil {
		t.Fatal("runWithArgs returned nil error")
	}
	if err.Error() != "usage: kubeconfig-proxy <add-context|credential|serve> [flags]" {
		t.Fatalf("error = %q, want usage error", err.Error())
	}
}

func TestCLIHelpers(t *testing.T) {
	if got, want := splitCSV(" alpha, beta ,, gamma "), []string{"alpha", "beta", "gamma"}; !slices.Equal(got, want) {
		t.Fatalf("splitCSV = %v, want %v", got, want)
	}
	if got := splitCSV("  "); got != nil {
		t.Fatalf("splitCSV blank = %v, want nil", got)
	}
	if got := durationLogValue(0); got != "disabled" {
		t.Fatalf("durationLogValue(0) = %q, want disabled", got)
	}
	if got := durationLogValue(2 * time.Second); got != "2s" {
		t.Fatalf("durationLogValue(2s) = %q, want 2s", got)
	}
}

func TestAddContextWritesStateAndKubeconfigExecContext(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	statePath := filepath.Join(t.TempDir(), "prod-proxy.yaml")
	if err := runWithArgs([]string{
		"add-context", "prod-proxy",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"--listen", "127.0.0.1:27443",
		"--contexts", "alpha",
		"--primary-context", "alpha",
		"--proxy-ttl", "3m",
		"--exec-command", "kubeconfig-proxy-test",
	}, nil); err != nil {
		t.Fatal(err)
	}

	profile, err := proxystate.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "prod-proxy" {
		t.Fatalf("profile name = %q, want prod-proxy", profile.Name)
	}
	if profile.Listen != "127.0.0.1:27443" {
		t.Fatalf("profile listen = %q, want fixed listen addr", profile.Listen)
	}
	if !slices.Equal(profile.Contexts, []string{"alpha"}) {
		t.Fatalf("profile contexts = %v, want [alpha]", profile.Contexts)
	}
	if profile.ProxyTTL != "3m0s" {
		t.Fatalf("profile proxyTTL = %q, want 3m0s", profile.ProxyTTL)
	}
	if profile.BearerToken == "" || profile.TLS.CertPEM == "" || profile.TLS.KeyPEM == "" {
		t.Fatal("profile should contain proxy token and TLS material")
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, want 0600", info.Mode().Perm())
	}

	config, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	context := config.Contexts["prod-proxy"]
	if context == nil {
		t.Fatal("prod-proxy context is missing")
	}
	if context.Namespace != "test-ns" {
		t.Fatalf("proxy namespace = %q, want primary namespace", context.Namespace)
	}
	cluster := config.Clusters[context.Cluster]
	if cluster == nil {
		t.Fatal("proxy cluster is missing")
	}
	if cluster.Server != "https://127.0.0.1:27443" {
		t.Fatalf("proxy cluster server = %q, want fixed HTTPS server", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != profile.TLS.CertPEM {
		t.Fatal("kubeconfig CA data should match state certificate")
	}
	auth := config.AuthInfos[context.AuthInfo]
	if auth == nil || auth.Exec == nil {
		t.Fatal("proxy auth exec config is missing")
	}
	if auth.Exec.Command != "kubeconfig-proxy-test" {
		t.Fatalf("exec command = %q, want kubeconfig-proxy-test", auth.Exec.Command)
	}
	if !slices.Equal(auth.Exec.Args, []string{"credential", "--state", statePath}) {
		t.Fatalf("exec args = %v, want credential state args", auth.Exec.Args)
	}
}

func TestAddContextResolvesExplicitZeroListenPort(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	statePath := filepath.Join(t.TempDir(), "kind-proxy.yaml")
	if err := runWithArgs([]string{
		"add-context", "kind-proxy",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"--listen", "127.0.0.1:0",
		"--contexts", "alpha",
		"--primary-context", "alpha",
	}, nil); err != nil {
		t.Fatal(err)
	}

	profile, err := proxystate.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(profile.Listen)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("state listen host = %q, want 127.0.0.1", host)
	}
	if port == "0" {
		t.Fatalf("state listen = %q, want non-zero port", profile.Listen)
	}

	config, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cluster := config.Clusters["kubeconfig-proxy/kind-proxy"]
	if cluster == nil {
		t.Fatal("proxy cluster is missing")
	}
	if cluster.Server != "https://"+profile.Listen {
		t.Fatalf("cluster server = %q, want https://%s", cluster.Server, profile.Listen)
	}
}

func TestResolveAddContextListenAddrPicksStablePort(t *testing.T) {
	addr, err := resolveAddContextListenAddr("")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	if port == "0" {
		t.Fatalf("addr = %q, want non-zero port", addr)
	}
}

func TestServeStateStopsAfterTTLWithoutRequests(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.test"}`))
	}))
	defer upstream.Close()

	sourcePath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	listenAddr, err := pickAvailableListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	_, certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	token := "state-token"
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "ttl-proxy",
		SourceKubeconfig: sourcePath,
		Listen:           listenAddr,
		Contexts:         []string{"alpha"},
		PrimaryContext:   "alpha",
		BearerToken:      token,
		ProxyTTL:         "500ms",
		TLS: proxystate.TLS{
			CertPEM: string(certPEM),
			KeyPEM:  string(keyPEM),
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout: "2s",
			Retries:        0,
			RetryBackoff:   "1ms",
		},
	}
	statePath := filepath.Join(t.TempDir(), "ttl-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runWithArgs([]string{"serve", "--state", statePath}, nil)
	}()
	if err := waitReady(profile, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop after proxyTTL")
	}
}

func TestWriteExecCredential(t *testing.T) {
	var buf bytes.Buffer
	expiration := time.Date(2026, 7, 16, 15, 30, 0, 0, time.UTC)
	if err := writeExecCredential(&buf, "secret-token", &expiration); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	status, ok := payload["status"].(map[string]any)
	if !ok {
		t.Fatalf("status payload = %#v", payload["status"])
	}
	if status["token"] != "secret-token" {
		t.Fatalf("token = %q, want secret-token", status["token"])
	}
	if status["expirationTimestamp"] != "2026-07-16T15:30:00Z" {
		t.Fatalf("expirationTimestamp = %q, want RFC3339 timestamp", status["expirationTimestamp"])
	}
}

func TestWriteExecCredentialOmitsExpirationWhenProxyTTLDisabled(t *testing.T) {
	var buf bytes.Buffer
	if err := writeExecCredential(&buf, "secret-token", nil); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	status, ok := payload["status"].(map[string]any)
	if !ok {
		t.Fatalf("status payload = %#v", payload["status"])
	}
	if _, ok := status["expirationTimestamp"]; ok {
		t.Fatalf("expirationTimestamp should be omitted when proxyTTL is disabled: %#v", status)
	}
}

func TestExecCredentialExpirationUsesProxyTTLWithSkew(t *testing.T) {
	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	expiration := execCredentialExpiration(now, 10*time.Minute)
	if expiration == nil {
		t.Fatal("expiration is nil")
	}
	if got, want := expiration.Sub(now), 9*time.Minute+50*time.Second; got != want {
		t.Fatalf("valid duration = %s, want %s", got, want)
	}
	if expiration := execCredentialExpiration(now, 0); expiration != nil {
		t.Fatalf("expiration = %v, want nil when proxyTTL is disabled", expiration)
	}
}

func TestGenerateBearerToken(t *testing.T) {
	token, err := generateBearerToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 {
		t.Fatalf("token length = %d, want 43 raw-url-base64 chars for 32 bytes", len(token))
	}
	if token == "" {
		t.Fatal("token is empty")
	}
}

func TestGenerateTLSCertificateIncludesListenHost(t *testing.T) {
	certificate, caData, keyData, err := generateTLSCertificate("127.0.0.1:9443")
	if err != nil {
		t.Fatal(err)
	}
	if len(certificate.Certificate) == 0 {
		t.Fatal("certificate chain is empty")
	}
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(caData) == 0 {
		t.Fatal("certificate authority PEM is empty")
	}
	if len(keyData) == 0 {
		t.Fatal("private key PEM is empty")
	}
	if !slices.ContainsFunc(parsed.IPAddresses, func(ip net.IP) bool {
		return ip.Equal(net.ParseIP("127.0.0.1"))
	}) {
		t.Fatalf("certificate IP SANs = %v, want 127.0.0.1", parsed.IPAddresses)
	}
}

func writeMainTestKubeconfig(t *testing.T, serverURL string, caData []byte) string {
	t.Helper()

	config := clientcmdapi.NewConfig()
	config.Clusters["cluster-alpha"] = &clientcmdapi.Cluster{
		Server:                   serverURL,
		CertificateAuthorityData: caData,
	}
	config.AuthInfos["user-alpha"] = &clientcmdapi.AuthInfo{Token: "source-token"}
	config.Contexts["alpha"] = &clientcmdapi.Context{
		Cluster:   "cluster-alpha",
		AuthInfo:  "user-alpha",
		Namespace: "test-ns",
	}
	config.CurrentContext = "alpha"

	path := filepath.Join(t.TempDir(), "source.yaml")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func mainTestServerCAData(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}
