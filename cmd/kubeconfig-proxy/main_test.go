package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestRunWithArgsWritesProxyKubeconfigAndServes(t *testing.T) {
	upstreamAuth := make(chan string, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("upstream path = %q, want /version", r.URL.Path)
		}
		upstreamAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gitVersion":"v1.test"}`))
	}))
	defer upstream.Close()

	sourcePath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	outputPath := filepath.Join(t.TempDir(), "proxy.yaml")
	stop := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	stopped := false
	defer func() {
		if stopped {
			return
		}
		stop <- os.Interrupt
		select {
		case <-errCh:
		case <-time.After(time.Second):
		}
	}()

	go func() {
		errCh <- runWithArgs([]string{
			"--kubeconfig", sourcePath,
			"--contexts", "alpha",
			"--primary-context", "alpha",
			"--output", outputPath,
			"--listen", "127.0.0.1:0",
			"--request-timeout", "2s",
			"--retries", "0",
		}, stop)
	}()

	proxyConfig := waitForProxyConfig(t, outputPath, errCh)
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("proxy config should exist while proxy is running: %v", err)
	}
	proxyCluster := proxyConfig.Clusters["proxy"]
	if proxyCluster == nil {
		t.Fatal("generated proxy cluster is missing")
	}
	proxyAuth := proxyConfig.AuthInfos["proxy"]
	if proxyAuth == nil || proxyAuth.Token == "" {
		t.Fatal("generated proxy auth token is missing")
	}
	if proxyConfig.Contexts["kubeconfig-proxy"].Namespace != "test-ns" {
		t.Fatalf("proxy namespace = %q, want test-ns", proxyConfig.Contexts["kubeconfig-proxy"].Namespace)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(proxyCluster.CertificateAuthorityData) {
		t.Fatal("generated certificate authority data is not valid PEM")
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: roots,
		}},
	}
	req, err := http.NewRequest(http.MethodGet, proxyCluster.Server+"/version", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+proxyAuth.Token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	if string(body) != `{"gitVersion":"v1.test"}` {
		t.Fatalf("proxy body = %q, want upstream version response", string(body))
	}

	select {
	case got := <-upstreamAuth:
		if got != "Bearer source-token" {
			t.Fatalf("upstream auth = %q, want source kubeconfig token", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream was not called")
	}

	stop <- os.Interrupt
	select {
	case err := <-errCh:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWithArgs did not stop")
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proxy config should be removed after shutdown, stat err = %v", err)
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

	t.Setenv("KUBECONFIG", "/tmp/source-config")
	if got := displayKubeconfigPath(""); got != "/tmp/source-config" {
		t.Fatalf("displayKubeconfigPath empty = %q, want KUBECONFIG", got)
	}
	if got := displayKubeconfigPath("/explicit"); got != "/explicit" {
		t.Fatalf("displayKubeconfigPath explicit = %q, want explicit path", got)
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
	certificate, caData, err := generateTLSCertificate("127.0.0.1:9443")
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
	if !slices.ContainsFunc(parsed.IPAddresses, func(ip net.IP) bool {
		return ip.Equal(net.ParseIP("127.0.0.1"))
	}) {
		t.Fatalf("certificate IP SANs = %v, want 127.0.0.1", parsed.IPAddresses)
	}
}

func waitForProxyConfig(t *testing.T, path string, errCh <-chan error) *clientcmdapi.Config {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("runWithArgs exited before writing proxy config: %v", err)
		default:
		}

		config, err := clientcmd.LoadFromFile(path)
		if err == nil && config.Clusters["proxy"] != nil {
			return config
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("proxy config was not written to %s", path)
	return nil
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
