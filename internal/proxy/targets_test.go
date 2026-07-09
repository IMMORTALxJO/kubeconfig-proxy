package proxy

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestLoadTargetsUsesSelectedContextsAndCurrentPrimary(t *testing.T) {
	seenAuth := map[string]string{}
	var seenAuthMu sync.Mutex
	serverFor := func(name string) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenAuthMu.Lock()
			seenAuth[name] = r.Header.Get("Authorization")
			seenAuthMu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
	}

	alpha := serverFor("alpha")
	defer alpha.Close()
	beta := serverFor("beta")
	defer beta.Close()

	kubeconfigPath := writeTargetsTestKubeconfig(t, []targetContext{
		{name: "alpha", server: alpha.URL, caData: serverCAData(alpha), token: "alpha-token", namespace: "alpha-ns"},
		{name: "beta", server: beta.URL, caData: serverCAData(beta), token: "beta-token", namespace: "beta-ns"},
	}, "beta")

	targets, primary, err := LoadTargets(kubeconfigPath, []string{"alpha", "beta"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := TargetNames(targets), "alpha, beta"; got != want {
		t.Fatalf("target names = %q, want %q", got, want)
	}
	if primary.Name != "beta" {
		t.Fatalf("primary = %q, want beta", primary.Name)
	}
	if targets[0].Namespace != "alpha-ns" || targets[1].Namespace != "beta-ns" {
		t.Fatalf("namespaces = %q, %q; want alpha-ns, beta-ns", targets[0].Namespace, targets[1].Namespace)
	}

	for _, target := range targets {
		resp, err := target.Client.Get(target.Host.String() + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	seenAuthMu.Lock()
	defer seenAuthMu.Unlock()
	if seenAuth["alpha"] != "Bearer alpha-token" {
		t.Fatalf("alpha auth = %q, want bearer token from kubeconfig", seenAuth["alpha"])
	}
	if seenAuth["beta"] != "Bearer beta-token" {
		t.Fatalf("beta auth = %q, want bearer token from kubeconfig", seenAuth["beta"])
	}
}

func TestLoadTargetsDefaultsToSortedContextsWhenCurrentContextIsEmpty(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	kubeconfigPath := writeTargetsTestKubeconfig(t, []targetContext{
		{name: "zeta", server: server.URL, caData: serverCAData(server), token: "zeta-token"},
		{name: "alpha", server: server.URL, caData: serverCAData(server), token: "alpha-token"},
	}, "")

	targets, primary, err := LoadTargets(kubeconfigPath, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	gotNames := []string{targets[0].Name, targets[1].Name}
	if want := []string{"alpha", "zeta"}; !slices.Equal(gotNames, want) {
		t.Fatalf("target order = %v, want %v", gotNames, want)
	}
	if primary.Name != "alpha" {
		t.Fatalf("primary = %q, want alphabetically first context", primary.Name)
	}
}

func TestLoadTargetsRejectsInvalidKubeconfigSelections(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	kubeconfigPath := writeTargetsTestKubeconfig(t, []targetContext{
		{name: "alpha", server: server.URL, caData: serverCAData(server), token: "alpha-token"},
		{name: "beta", server: server.URL, caData: serverCAData(server), token: "beta-token"},
	}, "alpha")

	tests := []struct {
		name            string
		path            string
		selected        []string
		primary         string
		wantErrContains string
	}{
		{
			name:            "missing selected context",
			path:            kubeconfigPath,
			selected:        []string{"missing"},
			wantErrContains: `context "missing" not found`,
		},
		{
			name:            "primary not selected",
			path:            kubeconfigPath,
			selected:        []string{"alpha"},
			primary:         "beta",
			wantErrContains: `primary context "beta" is not included`,
		},
		{
			name:            "no contexts",
			path:            writeTargetsTestKubeconfig(t, nil, ""),
			wantErrContains: "source kubeconfig has no contexts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := LoadTargets(tt.path, tt.selected, tt.primary)
			if err == nil {
				t.Fatal("LoadTargets returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

type targetContext struct {
	name      string
	server    string
	caData    []byte
	token     string
	namespace string
}

func writeTargetsTestKubeconfig(t *testing.T, contexts []targetContext, currentContext string) string {
	t.Helper()

	config := clientcmdapi.NewConfig()
	for _, context := range contexts {
		clusterName := "cluster-" + context.name
		authName := "user-" + context.name
		config.Clusters[clusterName] = &clientcmdapi.Cluster{
			Server:                   context.server,
			CertificateAuthorityData: context.caData,
		}
		config.AuthInfos[authName] = &clientcmdapi.AuthInfo{Token: context.token}
		config.Contexts[context.name] = &clientcmdapi.Context{
			Cluster:   clusterName,
			AuthInfo:  authName,
			Namespace: context.namespace,
		}
	}
	config.CurrentContext = currentContext

	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func serverCAData(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}
