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

func TestSourceSelectContexts(t *testing.T) {
	source := loadSelectionTestSource(t, []string{"alpha", "beta", "managed-proxy", "managed-proxy-two", "prod-west", "proxy"}, "beta")
	for _, contextName := range []string{"managed-proxy", "managed-proxy-two", "proxy"} {
		managedEntryName := formatProxyEntryName(contextName)
		source.rawConfig.Contexts[contextName].Cluster = managedEntryName
		source.rawConfig.Contexts[contextName].AuthInfo = managedEntryName
	}
	tests := []struct {
		name            string
		selection       ContextSelection
		wantContexts    []string
		wantPrimary     string
		wantErrContains string
	}{
		{
			name: "explicit contexts keep selected order and current primary",
			selection: ContextSelection{
				ProxyContextName: "proxy",
				SelectedContexts: []string{"prod-west", "beta"},
			},
			wantContexts: []string{"prod-west", "beta"},
			wantPrimary:  "beta",
		},
		{
			name: "regexp selects sorted contexts excluding proxy context",
			selection: ContextSelection{
				ProxyContextName: "proxy",
				ContextRegexp:    "^prod|alpha$",
			},
			wantContexts: []string{"alpha", "prod-west"},
			wantPrimary:  "alpha",
		},
		{
			name: "explicit primary overrides current context",
			selection: ContextSelection{
				ProxyContextName: "proxy",
				SelectedContexts: []string{"alpha", "prod-west"},
				PrimaryContext:   "prod-west",
			},
			wantContexts: []string{"alpha", "prod-west"},
			wantPrimary:  "prod-west",
		},
		{
			name: "explicit contexts and regexp are combined without duplicates",
			selection: ContextSelection{
				SelectedContexts: []string{"prod-west", "alpha"},
				ContextRegexp:    "^(alpha|beta)$",
			},
			wantContexts: []string{"prod-west", "alpha", "beta"},
			wantPrimary:  "beta",
		},
		{
			name: "regexp excludes all managed proxy contexts",
			selection: ContextSelection{
				ProxyContextName: "new-proxy",
				ContextRegexp:    ".*",
			},
			wantContexts: []string{"alpha", "beta", "prod-west"},
			wantPrimary:  "beta",
		},
		{
			name: "explicit contexts include managed proxy contexts",
			selection: ContextSelection{
				ProxyContextName: "proxy",
				SelectedContexts: []string{"managed-proxy"},
			},
			wantContexts: []string{"managed-proxy"},
			wantPrimary:  "managed-proxy",
		},
		{
			name: "explicit managed proxy context complements regexp",
			selection: ContextSelection{
				ProxyContextName: "proxy",
				SelectedContexts: []string{"managed-proxy"},
				ContextRegexp:    ".*",
			},
			wantContexts: []string{"managed-proxy", "alpha", "beta", "prod-west"},
			wantPrimary:  "beta",
		},
		{
			name:            "invalid regexp",
			selection:       ContextSelection{ContextRegexp: "["},
			wantErrContains: "missing closing ]",
		},
		{
			name:            "missing selected context",
			selection:       ContextSelection{SelectedContexts: []string{"missing"}},
			wantErrContains: `context "missing" not found`,
		},
		{
			name: "selected proxy context",
			selection: ContextSelection{
				ProxyContextName: "proxy",
				SelectedContexts: []string{"proxy"},
			},
			wantErrContains: `source contexts must not include proxy context "proxy"`,
		},
		{
			name:            "duplicate selected context",
			selection:       ContextSelection{SelectedContexts: []string{"alpha", "alpha"}},
			wantErrContains: `context "alpha" is selected more than once`,
		},
		{
			name: "primary outside selected contexts",
			selection: ContextSelection{
				SelectedContexts: []string{"alpha"},
				PrimaryContext:   "beta",
			},
			wantErrContains: `primary context "beta" is not included`,
		},
		{
			name: "empty regexp selection",
			selection: ContextSelection{
				ProxyContextName: "proxy",
				ContextRegexp:    "^missing$",
			},
			wantErrContains: "no source contexts selected",
		},
		{
			name:         "default selects sorted contexts excluding proxy context",
			selection:    ContextSelection{ProxyContextName: "proxy"},
			wantContexts: []string{"alpha", "beta", "prod-west"},
			wantPrimary:  "beta",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contexts, primary, err := source.SelectContexts(tt.selection)
			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatal("SelectContexts returned nil error")
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(contexts, tt.wantContexts) {
				t.Fatalf("contexts = %v, want %v", contexts, tt.wantContexts)
			}
			if primary != tt.wantPrimary {
				t.Fatalf("primary = %q, want %q", primary, tt.wantPrimary)
			}
		})
	}
}

func TestLoadSourceReturnsLoadErrors(t *testing.T) {
	if _, err := LoadSource(filepath.Join(t.TempDir(), "missing.yaml")); !os.IsNotExist(err) {
		t.Fatalf("missing source error = %v, want not-exist error", err)
	}
	if _, err := LoadSource(writeSelectionTestKubeconfig(t, nil, "")); err == nil || !strings.Contains(err.Error(), "source kubeconfig has no contexts") {
		t.Fatalf("empty source error = %v, want no contexts error", err)
	}
}

func TestSourceResolvesContextsAndClientConfig(t *testing.T) {
	source := loadSelectionTestSource(t, []string{"beta", "alpha"}, "beta")

	contexts, err := source.ResolveContexts(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "beta"}; !slices.Equal(contexts, want) {
		t.Fatalf("resolved contexts = %v, want %v", contexts, want)
	}

	contexts, err = source.ResolveContexts([]string{"beta", "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"beta", "alpha"}; !slices.Equal(contexts, want) {
		t.Fatalf("explicit contexts = %v, want %v", contexts, want)
	}
	if _, err := source.ResolveContexts([]string{"alpha", "alpha"}); err == nil || !strings.Contains(err.Error(), "selected more than once") {
		t.Fatalf("duplicate contexts error = %v", err)
	}
	if got := source.CurrentContext(); got != "beta" {
		t.Fatalf("current context = %q, want beta", got)
	}

	kubeContext, restConfig, err := source.ClientConfig("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if kubeContext.Cluster != "cluster-alpha" {
		t.Fatalf("context cluster = %q, want cluster-alpha", kubeContext.Cluster)
	}
	if restConfig.Host != "https://alpha.example.test" {
		t.Fatalf("REST host = %q, want alpha server", restConfig.Host)
	}
	if _, _, err := source.ClientConfig("missing"); err == nil || !strings.Contains(err.Error(), `context "missing" not found`) {
		t.Fatalf("missing client config error = %v", err)
	}
}

func loadSelectionTestSource(t *testing.T, contextNames []string, currentContext string) *Source {
	t.Helper()
	source, err := LoadSource(writeSelectionTestKubeconfig(t, contextNames, currentContext))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func writeSelectionTestKubeconfig(t *testing.T, contextNames []string, currentContext string) string {
	t.Helper()
	config := clientcmdapi.NewConfig()
	for _, name := range contextNames {
		clusterName := "cluster-" + name
		config.Clusters[clusterName] = &clientcmdapi.Cluster{Server: "https://" + name + ".example.test"}
		config.Contexts[name] = &clientcmdapi.Context{Cluster: clusterName}
	}
	config.CurrentContext = currentContext
	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
	return path
}
