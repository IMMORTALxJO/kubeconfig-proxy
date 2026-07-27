package upstream

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
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

	source := loadTargetTestSource(t, kubeconfigPath)
	targets, primary, err := LoadTargets(source, []string{"alpha", "beta"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := NameList(targets), []string{"alpha", "beta"}; !slices.Equal(got, want) {
		t.Fatalf("target name list = %v, want %v", got, want)
	}
	if got, want := Names(targets), "alpha, beta"; got != want {
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

func TestLoadTargetsSupportsOIDCAuthProvider(t *testing.T) {
	var seenAuth string
	var seenAuthMu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthMu.Lock()
		seenAuth = r.Header.Get("Authorization")
		seenAuthMu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	idToken := validOIDCTestToken()
	kubeconfigPath := writeTargetsTestKubeconfig(t, []targetContext{
		{
			name:   "oidc",
			server: server.URL,
			caData: serverCAData(server),
			authProvider: &clientcmdapi.AuthProviderConfig{
				Name: "oidc",
				Config: map[string]string{
					"idp-issuer-url": "https://issuer.example.test",
					"client-id":      "kubeconfig-proxy-test",
					"id-token":       idToken,
				},
			},
		},
	}, "oidc")

	source := loadTargetTestSource(t, kubeconfigPath)
	targets, primary, err := LoadTargets(source, []string{"oidc"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Name != "oidc" {
		t.Fatalf("primary = %q, want oidc", primary.Name)
	}
	resp, err := targets[0].Client.Get(targets[0].Host.String() + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	seenAuthMu.Lock()
	defer seenAuthMu.Unlock()
	if seenAuth != "Bearer "+idToken {
		t.Fatalf("auth = %q, want OIDC id-token from kubeconfig", seenAuth)
	}
}

func TestLoadTargetsDefaultsToSortedContextsWhenCurrentContextIsEmpty(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	kubeconfigPath := writeTargetsTestKubeconfig(t, []targetContext{
		{name: "zeta", server: server.URL, caData: serverCAData(server), token: "zeta-token"},
		{name: "alpha", server: server.URL, caData: serverCAData(server), token: "alpha-token"},
	}, "")

	source := loadTargetTestSource(t, kubeconfigPath)
	targets, primary, err := LoadTargets(source, nil, "")
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
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
			name:            "duplicate selected context",
			path:            kubeconfigPath,
			selected:        []string{"alpha", "alpha"},
			wantErrContains: `context "alpha" is selected more than once`,
		},
		{
			name:            "no contexts",
			path:            writeTargetsTestKubeconfig(t, nil, ""),
			wantErrContains: "source kubeconfig has no contexts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, loadErr := kubeconfig.LoadSource(tt.path)
			if loadErr != nil {
				if !strings.Contains(loadErr.Error(), tt.wantErrContains) {
					t.Fatalf("load error = %q, want to contain %q", loadErr.Error(), tt.wantErrContains)
				}
				return
			}
			_, _, err := LoadTargets(source, tt.selected, tt.primary)
			if err == nil {
				t.Fatal("LoadTargets returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}

	if _, _, err := LoadTargets(nil, nil, ""); err == nil || !strings.Contains(err.Error(), "source kubeconfig is required") {
		t.Fatalf("nil source error = %v, want source kubeconfig error", err)
	}
}

type targetContext struct {
	name         string
	server       string
	caData       []byte
	token        string
	authProvider *clientcmdapi.AuthProviderConfig
	namespace    string
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
		config.AuthInfos[authName] = &clientcmdapi.AuthInfo{
			Token:        context.token,
			AuthProvider: context.authProvider,
		}
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

func loadTargetTestSource(t *testing.T, path string) *kubeconfig.Source {
	t.Helper()
	source, err := kubeconfig.LoadSource(path)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func validOIDCTestToken() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	return header + "." + payload + "."
}

func serverCAData(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}
