package kubeconfig

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestAddProxyContextWritesExecContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	config := clientcmdapi.NewConfig()
	config.Clusters["source-cluster"] = &clientcmdapi.Cluster{Server: "https://source.example.test"}
	config.AuthInfos["source-user"] = &clientcmdapi.AuthInfo{Token: "source-token"}
	config.Contexts["source"] = &clientcmdapi.Context{Cluster: "source-cluster", AuthInfo: "source-user"}
	config.CurrentContext = "source"
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}

	caData := []byte("test-ca")
	if err := AddProxyContext(path, "prod-proxy", "https://127.0.0.1:9443", "default", "/usr/local/bin/kubeconfig-proxy", "/tmp/prod-proxy.yaml", caData); err != nil {
		t.Fatal(err)
	}

	got, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Contexts["source"] == nil {
		t.Fatal("source context should be preserved")
	}

	context := got.Contexts["prod-proxy"]
	if context == nil {
		t.Fatal("proxy context is missing")
	}
	if context.Cluster != "kubeconfig-proxy/prod-proxy" {
		t.Fatalf("context cluster = %q, want proxy cluster", context.Cluster)
	}
	if context.AuthInfo != "kubeconfig-proxy/prod-proxy" {
		t.Fatalf("context auth info = %q, want proxy auth info", context.AuthInfo)
	}
	if context.Namespace != "default" {
		t.Fatalf("context namespace = %q, want default", context.Namespace)
	}

	cluster := got.Clusters["kubeconfig-proxy/prod-proxy"]
	if cluster == nil {
		t.Fatal("proxy cluster is missing")
	}
	if cluster.Server != "https://127.0.0.1:9443" {
		t.Fatalf("cluster server = %q, want proxy server", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != "test-ca" {
		t.Fatalf("certificate authority data = %q, want test-ca", string(cluster.CertificateAuthorityData))
	}

	authInfo := got.AuthInfos["kubeconfig-proxy/prod-proxy"]
	if authInfo == nil || authInfo.Exec == nil {
		t.Fatal("proxy exec auth info is missing")
	}
	if authInfo.Exec.APIVersion != execAPIVersion {
		t.Fatalf("exec api version = %q, want %q", authInfo.Exec.APIVersion, execAPIVersion)
	}
	if authInfo.Exec.Command != "/usr/local/bin/kubeconfig-proxy" {
		t.Fatalf("exec command = %q, want binary path", authInfo.Exec.Command)
	}
	if !slices.Equal(authInfo.Exec.Args, []string{"credential", "--state", "/tmp/prod-proxy.yaml"}) {
		t.Fatalf("exec args = %v, want credential state args", authInfo.Exec.Args)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
