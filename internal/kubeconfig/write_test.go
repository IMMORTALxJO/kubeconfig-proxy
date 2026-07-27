package kubeconfig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	assertProxyContext(t, got)
	assertProxyCluster(t, got)
	assertProxyAuthInfo(t, got)
	assertKubeconfigFileMode(t, path)
}

func assertProxyContext(t *testing.T, config *clientcmdapi.Config) {
	t.Helper()

	context := config.Contexts["prod-proxy"]
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
}

func assertProxyCluster(t *testing.T, config *clientcmdapi.Config) {
	t.Helper()

	cluster := config.Clusters["kubeconfig-proxy/prod-proxy"]
	if cluster == nil {
		t.Fatal("proxy cluster is missing")
	}
	if cluster.Server != "https://127.0.0.1:9443" {
		t.Fatalf("cluster server = %q, want proxy server", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != "test-ca" {
		t.Fatalf("certificate authority data = %q, want test-ca", string(cluster.CertificateAuthorityData))
	}
}

func assertProxyAuthInfo(t *testing.T, config *clientcmdapi.Config) {
	t.Helper()

	authInfo := config.AuthInfos["kubeconfig-proxy/prod-proxy"]
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
}

func assertKubeconfigFileMode(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestAddProxyContextCreatesMissingKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config")

	err := AddProxyContext(path, "prod-proxy", "https://127.0.0.1:9443", "", "kubeconfig-proxy", "/tmp/prod.yaml", []byte("ca"))
	if err != nil {
		t.Fatal(err)
	}

	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Contexts["prod-proxy"] == nil {
		t.Fatal("proxy context is missing")
	}
}

func TestAddProxyContextReturnsLoadAndWriteErrors(t *testing.T) {
	t.Run("invalid existing kubeconfig", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(path, []byte("contexts: ["), 0o600); err != nil {
			t.Fatal(err)
		}

		err := AddProxyContext(path, "prod-proxy", "https://127.0.0.1:9443", "", "kubeconfig-proxy", "/tmp/prod.yaml", []byte("ca"))
		if err == nil {
			t.Fatal("AddProxyContext returned nil error")
		}
	})

	t.Run("parent path is file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-dir")
		if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := AddProxyContext(filepath.Join(parent, "config"), "prod-proxy", "https://127.0.0.1:9443", "", "kubeconfig-proxy", "/tmp/prod.yaml", []byte("ca"))
		if err == nil {
			t.Fatal("AddProxyContext returned nil error")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("error = %q, want not a directory", err.Error())
		}
	})
}

func TestDeleteProxyContextRemovesManagedEntriesAndReturnsStatePath(t *testing.T) {
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

	statePaths, err := DeleteProxyContext(path, "prod-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(statePaths, []string{"/tmp/prod-proxy.yaml"}) {
		t.Fatalf("state paths = %v, want [/tmp/prod-proxy.yaml]", statePaths)
	}

	got, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Contexts["source"] == nil {
		t.Fatal("source context should be preserved")
	}
	if got.Contexts["prod-proxy"] != nil {
		t.Fatal("proxy context should be removed")
	}
	if got.Clusters["kubeconfig-proxy/prod-proxy"] != nil {
		t.Fatal("proxy cluster should be removed")
	}
	if got.AuthInfos["kubeconfig-proxy/prod-proxy"] != nil {
		t.Fatal("proxy auth info should be removed")
	}
}

func TestDeleteProxyContextHandlesMissingAndUnchangedKubeconfigs(t *testing.T) {
	t.Run("missing kubeconfig", func(t *testing.T) {
		statePaths, err := DeleteProxyContext(filepath.Join(t.TempDir(), "missing"), "prod-proxy")
		if err != nil {
			t.Fatal(err)
		}
		if statePaths != nil {
			t.Fatalf("state paths = %v, want nil", statePaths)
		}
	})

	t.Run("no managed entries", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		config := clientcmdapi.NewConfig()
		config.Clusters["source-cluster"] = &clientcmdapi.Cluster{Server: "https://source.example.test"}
		config.AuthInfos["source-user"] = &clientcmdapi.AuthInfo{Token: "source-token"}
		config.Contexts["source"] = &clientcmdapi.Context{Cluster: "source-cluster", AuthInfo: "source-user"}
		if err := clientcmd.WriteToFile(*config, path); err != nil {
			t.Fatal(err)
		}

		statePaths, err := DeleteProxyContext(path, "prod-proxy")
		if err != nil {
			t.Fatal(err)
		}
		if len(statePaths) != 0 {
			t.Fatalf("state paths = %v, want empty", statePaths)
		}
	})
}

func TestDeleteProxyContextClearsCurrentContextAndParsesStateEqualsArg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	config := clientcmdapi.NewConfig()
	entryName := proxyEntryName("prod-proxy")
	config.Clusters[entryName] = &clientcmdapi.Cluster{Server: "https://127.0.0.1:9443"}
	config.AuthInfos[entryName] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		Args: []string{"credential", "--state=/tmp/prod-proxy.yaml", "--state", "/tmp/prod-proxy.yaml"},
	}}
	config.Contexts["prod-proxy"] = &clientcmdapi.Context{Cluster: entryName, AuthInfo: entryName}
	config.CurrentContext = "prod-proxy"
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}

	statePaths, err := DeleteProxyContext(path, "prod-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(statePaths, []string{"/tmp/prod-proxy.yaml"}) {
		t.Fatalf("state paths = %v, want deduplicated state path", statePaths)
	}

	got, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentContext != "" {
		t.Fatalf("current context = %q, want cleared", got.CurrentContext)
	}
}

func TestDeleteProxyContextRejectsUnmanagedContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	config := clientcmdapi.NewConfig()
	config.Clusters["source-cluster"] = &clientcmdapi.Cluster{Server: "https://source.example.test"}
	config.AuthInfos["source-user"] = &clientcmdapi.AuthInfo{Token: "source-token"}
	config.Contexts["prod-proxy"] = &clientcmdapi.Context{Cluster: "source-cluster", AuthInfo: "source-user"}
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}

	if _, err := DeleteProxyContext(path, "prod-proxy"); err == nil {
		t.Fatal("DeleteProxyContext returned nil error, want unmanaged context error")
	}
}

func TestAuthInfoStatePathsHandlesMissingExec(t *testing.T) {
	if got := authInfoStatePaths(nil); got != nil {
		t.Fatalf("nil auth info paths = %v, want nil", got)
	}
	if got := authInfoStatePaths(&clientcmdapi.AuthInfo{}); got != nil {
		t.Fatalf("auth info without exec paths = %v, want nil", got)
	}
}

func TestAppendUniquePaths(t *testing.T) {
	got := AppendUniquePaths([]string{"alpha"}, "", "alpha", "beta")
	if !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("AppendUniquePaths = %v, want [alpha beta]", got)
	}
}
