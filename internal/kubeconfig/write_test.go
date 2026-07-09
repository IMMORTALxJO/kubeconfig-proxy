package kubeconfig

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestWriteProxyConfigIncludesBearerToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	caData := []byte("test-ca")
	if err := WriteProxyConfig(path, "https://127.0.0.1:9443", "kubeconfig-proxy", "default", "secret-token", caData); err != nil {
		t.Fatal(err)
	}

	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}

	authInfo := config.AuthInfos["proxy"]
	if authInfo == nil {
		t.Fatal("proxy auth info is missing")
	}
	if authInfo.Token != "secret-token" {
		t.Fatalf("token = %q, want secret-token", authInfo.Token)
	}

	cluster := config.Clusters["proxy"]
	if cluster == nil {
		t.Fatal("proxy cluster is missing")
	}
	if string(cluster.CertificateAuthorityData) != string(caData) {
		t.Fatalf("certificate authority data = %q, want %q", string(cluster.CertificateAuthorityData), string(caData))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
