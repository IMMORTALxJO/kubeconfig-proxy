package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestRunWithArgsRequiresSubcommand(t *testing.T) {
	err := runWithArgs(nil, nil)
	if err == nil {
		t.Fatal("runWithArgs returned nil error")
	}
	if err.Error() != "usage: kubeconfig-proxy <add-context|delete-context|credential|serve|version> [flags]" {
		t.Fatalf("error = %q, want usage error", err.Error())
	}
}

func TestRunVersionWritesDevByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := runVersion(nil, &buf); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRunVersionWritesInjectedVersion(t *testing.T) {
	oldVersion := cliVersion
	cliVersion = "v1.2.3"
	t.Cleanup(func() {
		cliVersion = oldVersion
	})

	var buf bytes.Buffer
	if err := runVersion(nil, &buf); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "v1.2.3\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRunVersionRejectsArgs(t *testing.T) {
	err := runVersion([]string{"extra"}, io.Discard)
	if err == nil {
		t.Fatal("runVersion returned nil error")
	}
	if err.Error() != "usage: kubeconfig-proxy version" {
		t.Fatalf("error = %q, want version usage", err.Error())
	}
}

func TestRunWithArgsDispatchesVersion(t *testing.T) {
	output := captureStdout(t, func() error {
		return runWithArgs([]string{"version"}, nil)
	})
	if got, want := output, "dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRunUsesOSArgs(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"kubeconfig-proxy", "version"}
	t.Cleanup(func() {
		os.Args = oldArgs
	})

	output := captureStdout(t, run)
	if got, want := output, "dev\n"; got != want {
		t.Fatalf("run output = %q, want %q", got, want)
	}
}

func TestCLIHelpers(t *testing.T) {
	if got, want := splitCSV(" alpha, beta ,, gamma "), []string{"alpha", "beta", "gamma"}; !slices.Equal(got, want) {
		t.Fatalf("splitCSV = %v, want %v", got, want)
	}
	if got := splitCSV("  "); got != nil {
		t.Fatalf("splitCSV blank = %v, want nil", got)
	}
	if got := formatDurationForLog(0); got != "disabled" {
		t.Fatalf("formatDurationForLog(0) = %q, want disabled", got)
	}
	if got := formatDurationForLog(2 * time.Second); got != "2s" {
		t.Fatalf("formatDurationForLog(2s) = %q, want 2s", got)
	}
	unsafeName := sanitizeFileName("prod/blue:west_1")
	if !strings.HasPrefix(unsafeName, "prod_blue_west_1-") {
		t.Fatalf("sanitizeFileName = %q, want readable prefix and hash", unsafeName)
	}
	if got := sanitizeFileName("Prod.Blue-1"); got != "Prod.Blue-1" {
		t.Fatalf("sanitizeFileName uppercase/dot/dash = %q, want Prod.Blue-1", got)
	}
	if got := sanitizeFileName("prod/blue"); got == sanitizeFileName("prod_blue") {
		t.Fatalf("unsafe and safe context names map to the same filename %q", got)
	}
	gotStatePath, err := resolveDefaultStatePath("prod/blue")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(".kube", "kubeconfig-proxy", sanitizeFileName("prod/blue")+".yaml"); !strings.HasSuffix(gotStatePath, want) {
		t.Fatalf("resolveDefaultStatePath = %q, want suffix %q", gotStatePath, want)
	}
}

func TestDefaultCommandAndLongStateNames(t *testing.T) {
	longName := strings.Repeat("a", 100)
	gotName := sanitizeFileName(longName)
	if len(gotName) != 93 {
		t.Fatalf("long safe filename length = %d, want 93", len(gotName))
	}
	if !strings.HasPrefix(gotName, strings.Repeat("a", 80)+"-") {
		t.Fatalf("long safe filename = %q, want truncated readable prefix", gotName)
	}
	if executable := resolveDefaultExecCommand(); executable == "" {
		t.Fatal("default executable is empty")
	}

	cmd := newDetachedServeCommand("kubeconfig-proxy-test", "/tmp/proxy-state.yaml")
	if want := []string{"kubeconfig-proxy-test", "serve", "--state", "/tmp/proxy-state.yaml"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("detached command args = %v, want %v", cmd.Args, want)
	}
}

func TestAddContextWritesStateAndKubeconfigExecContext(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	profile := assertMainTestProxyState(t, statePath)
	if profile.SourceKubeconfig != kubeconfigPath {
		t.Fatalf("profile source kubeconfig = %q, want %q", profile.SourceKubeconfig, kubeconfigPath)
	}
	assertMainTestProxyKubeconfig(t, kubeconfigPath, statePath, profile)
}

func TestAddContextUsesDefaultKubeconfigLoadingAfterFileMove(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	t.Setenv("KUBECONFIG", kubeconfigPath)
	statePath := filepath.Join(t.TempDir(), "dynamic-proxy.yaml")
	if err := runWithArgs([]string{
		"add-context", "dynamic-proxy",
		"--state", statePath,
		"--listen", "127.0.0.1:0",
		"--contexts", "alpha",
	}, nil); err != nil {
		t.Fatal(err)
	}

	profile := loadMainTestProfile(t, statePath)
	if profile.SourceKubeconfig != "" {
		t.Fatalf("profile source kubeconfig = %q, want default loading", profile.SourceKubeconfig)
	}
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateData), "sourceKubeconfig:") {
		t.Fatalf("dynamic state should omit sourceKubeconfig:\n%s", stateData)
	}

	movedKubeconfigPath := filepath.Join(t.TempDir(), "moved-kubeconfig")
	if err := os.Rename(kubeconfigPath, movedKubeconfigPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", movedKubeconfigPath)
	if _, _, err := loadServeRuntime(statePath); err != nil {
		t.Fatalf("loadServeRuntime after kubeconfig move: %v", err)
	}
}

func TestAddContextPersistsContextSelectors(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	tests := []struct {
		name         string
		selectorArgs []string
		want         proxystate.ContextSelection
	}{
		{
			name: "default regexp",
			want: proxystate.ContextSelection{Regexp: ".*"},
		},
		{
			name:         "regexp only",
			selectorArgs: []string{"--context-regexp", "^prod-.*"},
			want:         proxystate.ContextSelection{Regexp: "^prod-.*"},
		},
		{
			name:         "names only",
			selectorArgs: []string{"--contexts", "alpha"},
			want:         proxystate.ContextSelection{Names: []string{"alpha"}},
		},
		{
			name: "combined selectors and primary",
			selectorArgs: []string{
				"--contexts", "alpha",
				"--context-regexp", "^prod-.*",
				"--primary-context", "prod-b",
			},
			want: proxystate.ContextSelection{
				Regexp:  "^prod-.*",
				Names:   []string{"alpha"},
				Primary: "prod-b",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kubeconfigPath := writeMainTestKubeconfigWithContexts(t, []mainTestContext{
				{name: "alpha", serverURL: upstream.URL, caData: mainTestServerCAData(upstream)},
				{name: "prod-a", serverURL: upstream.URL, caData: mainTestServerCAData(upstream)},
				{name: "prod-b", serverURL: upstream.URL, caData: mainTestServerCAData(upstream)},
			})
			statePath := filepath.Join(t.TempDir(), "selectors.yaml")
			args := []string{
				"add-context", "proxy",
				"--kubeconfig", kubeconfigPath,
				"--state", statePath,
				"--listen", "127.0.0.1:0",
			}
			args = append(args, test.selectorArgs...)
			if err := runWithArgs(args, nil); err != nil {
				t.Fatal(err)
			}

			profile := loadMainTestProfile(t, statePath)
			if profile.Contexts.Regexp != test.want.Regexp ||
				!slices.Equal(profile.Contexts.Names, test.want.Names) ||
				profile.Contexts.Primary != test.want.Primary {
				t.Fatalf("state contexts = %#v, want %#v", profile.Contexts, test.want)
			}
			data, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			stateYAML := string(data)
			if len(test.want.Names) == 0 && strings.Contains(stateYAML, "  names:") {
				t.Fatalf("state should omit unresolved names:\n%s", stateYAML)
			}
			if test.want.Primary == "" && strings.Contains(stateYAML, "  primary:") {
				t.Fatalf("state should omit inferred primary:\n%s", stateYAML)
			}
		})
	}
}

func assertMainTestProxyState(t *testing.T, statePath string) *proxystate.Profile {
	t.Helper()

	profile := loadMainTestProfile(t, statePath)
	assertMainTestProxyStateCore(t, profile)
	assertMainTestProxyStateDefaults(t, profile)
	assertFileMode(t, statePath, 0o600)
	return profile
}

func loadMainTestProfile(t *testing.T, statePath string) *proxystate.Profile {
	t.Helper()
	runtime, err := proxystate.LoadRuntime(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.Profile
}

func assertMainTestProxyStateCore(t *testing.T, profile *proxystate.Profile) {
	t.Helper()

	if profile.Name != "prod-proxy" {
		t.Fatalf("profile name = %q, want prod-proxy", profile.Name)
	}
	if profile.Listen != "127.0.0.1:27443" {
		t.Fatalf("profile listen = %q, want fixed listen addr", profile.Listen)
	}
	if !slices.Equal(profile.Contexts.Names, []string{"alpha"}) || profile.Contexts.Primary != "alpha" {
		t.Fatalf("profile contexts = %#v, want explicit alpha selection and primary", profile.Contexts)
	}
	if profile.ProxyTTL != "3m0s" {
		t.Fatalf("profile proxyTTL = %q, want 3m0s", profile.ProxyTTL)
	}
	if profile.BearerToken == "" || profile.TLS.CertPEM == "" || profile.TLS.KeyPEM == "" {
		t.Fatal("profile should contain proxy token and TLS material")
	}
}

func assertMainTestProxyStateDefaults(t *testing.T, profile *proxystate.Profile) {
	t.Helper()

	if profile.LogsEnabled {
		t.Fatal("profile logsEnabled = true, want false by default")
	}
	if profile.Options.ReadOnly {
		t.Fatal("profile readOnly = true, want false by default")
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode = %v, want %v", got, want)
	}
}

func assertMainTestProxyKubeconfig(t *testing.T, kubeconfigPath, statePath string, profile *proxystate.Profile) {
	t.Helper()

	config, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	context := config.Contexts["prod-proxy"]
	assertMainTestProxyContext(t, context)
	assertMainTestProxyCluster(t, config, context.Cluster, profile)
	assertMainTestProxyAuth(t, config, context.AuthInfo, statePath)
}

func assertMainTestProxyContext(t *testing.T, context *clientcmdapi.Context) {
	t.Helper()

	if context == nil {
		t.Fatal("prod-proxy context is missing")
	}
	if context.Namespace != "test-ns" {
		t.Fatalf("proxy namespace = %q, want primary namespace", context.Namespace)
	}
}

func assertMainTestProxyCluster(t *testing.T, config *clientcmdapi.Config, clusterName string, profile *proxystate.Profile) {
	t.Helper()

	cluster := config.Clusters[clusterName]
	if cluster == nil {
		t.Fatal("proxy cluster is missing")
	}
	if cluster.Server != "https://127.0.0.1:27443" {
		t.Fatalf("proxy cluster server = %q, want fixed HTTPS server", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != profile.TLS.CertPEM {
		t.Fatal("kubeconfig CA data should match state certificate")
	}
}

func assertMainTestProxyAuth(t *testing.T, config *clientcmdapi.Config, authInfoName, statePath string) {
	t.Helper()

	auth := config.AuthInfos[authInfoName]
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

func TestAddContextWritesLogsEnabledState(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	statePath := filepath.Join(t.TempDir(), "logs-proxy.yaml")
	if err := runWithArgs([]string{
		"add-context", "logs-proxy",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"--listen", "127.0.0.1:27444",
		"--contexts", "alpha",
		"--primary-context", "alpha",
		"--logs-enabled",
	}, nil); err != nil {
		t.Fatal(err)
	}

	profile := loadMainTestProfile(t, statePath)
	if !profile.LogsEnabled {
		t.Fatal("profile logsEnabled = false, want true")
	}
}

func TestAddContextWritesReadOnlyState(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	statePath := filepath.Join(t.TempDir(), "readonly-proxy.yaml")
	if err := runWithArgs([]string{
		"add-context", "readonly-proxy",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"--listen", "127.0.0.1:27446",
		"--contexts", "alpha",
		"--primary-context", "alpha",
		"--read-only",
	}, nil); err != nil {
		t.Fatal(err)
	}

	profile := loadMainTestProfile(t, statePath)
	if !profile.Options.ReadOnly {
		t.Fatal("profile readOnly = false, want true")
	}
}

func TestAddContextResolvesExplicitZeroListenPort(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	profile := loadMainTestProfile(t, statePath)
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

func TestAddContextAcceptsContextNameAfterFlagsAndDefaultStatePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	if err := runWithArgs([]string{
		"add-context",
		"--kubeconfig", kubeconfigPath,
		"--listen", "127.0.0.1:0",
		"--contexts", "alpha",
		"--primary-context", "alpha",
		"flag-first-proxy",
	}, nil); err != nil {
		t.Fatal(err)
	}

	statePath := mustDefaultStatePath(t, "flag-first-proxy")
	profile := loadMainTestProfile(t, statePath)
	if profile.Name != "flag-first-proxy" {
		t.Fatalf("profile name = %q, want flag-first-proxy", profile.Name)
	}
}

func TestAddContextRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantErrContains string
	}{
		{
			name:            "missing context name",
			args:            []string{"add-context"},
			wantErrContains: "usage: kubeconfig-proxy add-context",
		},
		{
			name:            "extra context name",
			args:            []string{"add-context", "one", "two"},
			wantErrContains: "usage: kubeconfig-proxy add-context",
		},
		{
			name:            "invalid proxy ttl flag",
			args:            []string{"add-context", "one", "--proxy-ttl", "nope"},
			wantErrContains: "invalid value",
		},
		{
			name:            "invalid listen address",
			args:            []string{"add-context", "one", "--listen", "bad-listen"},
			wantErrContains: "missing port in address",
		},
		{
			name:            "missing kubeconfig",
			args:            []string{"add-context", "one", "--kubeconfig", filepath.Join(t.TempDir(), "missing"), "--listen", "127.0.0.1:0"},
			wantErrContains: "no such file or directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithArgs(tt.args, nil)
			if err == nil {
				t.Fatal("runWithArgs returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

func TestAddContextRejectsDuplicateContextsBeforeWritingState(t *testing.T) {
	kubeconfigPath := writeMainTestKubeconfigWithContexts(t, []mainTestContext{
		{name: "alpha", serverURL: "https://alpha.example.test"},
	})
	statePath := filepath.Join(t.TempDir(), "duplicate.yaml")
	err := runWithArgs([]string{
		"add-context", "duplicate-proxy",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"--listen", "127.0.0.1:0",
		"--contexts", "alpha,alpha",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), `context "alpha" is selected more than once`) {
		t.Fatalf("error = %v, want duplicate context error", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state stat error = %v, want state not written", err)
	}
}

func TestDeleteContextRemovesKubeconfigAndStateArtifacts(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	statePath := filepath.Join(t.TempDir(), "prod-proxy.yaml")
	if err := runWithArgs([]string{
		"add-context", "prod-proxy",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"--listen", "127.0.0.1:27445",
		"--contexts", "alpha",
		"--primary-context", "alpha",
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{statePath + ".log", statePath + ".lock"} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := runWithArgs([]string{
		"delete-context", "prod-proxy",
		"--kubeconfig", kubeconfigPath,
	}, nil); err != nil {
		t.Fatal(err)
	}

	config, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Contexts["alpha"] == nil {
		t.Fatal("source context should be preserved")
	}
	if config.Contexts["prod-proxy"] != nil {
		t.Fatal("proxy context should be removed")
	}
	if config.Clusters["kubeconfig-proxy/prod-proxy"] != nil {
		t.Fatal("proxy cluster should be removed")
	}
	if config.AuthInfos["kubeconfig-proxy/prod-proxy"] != nil {
		t.Fatal("proxy auth info should be removed")
	}
	for _, path := range []string{statePath, statePath + ".log"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s stat error = %v, want not exists", path, err)
		}
	}
	if _, err := os.Stat(statePath + ".lock"); err != nil {
		t.Fatalf("%s.lock stat error = %v, want lock file preserved", statePath, err)
	}
}

func TestDeleteContextAcceptsContextAfterFlagsAndExplicitState(t *testing.T) {
	kubeconfigPath := filepath.Join(t.TempDir(), "missing-kubeconfig")
	statePath := filepath.Join(t.TempDir(), "extra-state.yaml")
	logPath := statePath + ".log"
	for _, path := range []string{statePath, logPath} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := runWithArgs([]string{
		"delete-context",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"flag-first-proxy",
	}, nil); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{statePath, logPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s stat error = %v, want not exists", path, err)
		}
	}
}

func TestDeleteContextUsesDefaultStateWhenKubeconfigHasNoManagedEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	kubeconfigPath := writeMainTestKubeconfigWithContexts(t, []mainTestContext{
		{name: "alpha", serverURL: "https://alpha.example.test"},
	})
	statePath := mustDefaultStatePath(t, "missing-proxy")
	logPath := statePath + ".log"
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{statePath, logPath} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := runWithArgs([]string{
		"delete-context", "missing-proxy",
		"--kubeconfig", kubeconfigPath,
	}, nil); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{statePath, logPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s stat error = %v, want not exists", path, err)
		}
	}
	if !strings.HasPrefix(statePath, home) {
		t.Fatalf("default state path = %q, want under test home %q", statePath, home)
	}
}

func TestDeleteContextRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantErrContains string
	}{
		{
			name:            "missing context name",
			args:            []string{"delete-context"},
			wantErrContains: "usage: kubeconfig-proxy delete-context",
		},
		{
			name:            "extra context name",
			args:            []string{"delete-context", "one", "two"},
			wantErrContains: "usage: kubeconfig-proxy delete-context",
		},
		{
			name:            "invalid flag",
			args:            []string{"delete-context", "--unknown"},
			wantErrContains: "flag provided but not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithArgs(tt.args, nil)
			if err == nil {
				t.Fatal("runWithArgs returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

func TestRemoveStateArtifactsReturnsUnexpectedRemoveError(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state-dir")
	if err := os.MkdirAll(filepath.Join(statePath, "child"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := removeStateArtifacts([]string{statePath})
	if err == nil {
		t.Fatal("removeStateArtifacts returned nil error")
	}
	if !strings.Contains(err.Error(), "directory not empty") {
		t.Fatalf("error = %q, want directory not empty", err.Error())
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

func TestResolveAddContextPaths(t *testing.T) {
	t.Run("default loading", func(t *testing.T) {
		kubeconfigPath := filepath.Join(t.TempDir(), "config")
		t.Setenv("KUBECONFIG", kubeconfigPath)
		resolvedPath, writePath, _, err := resolveAddContextPaths("", filepath.Join(t.TempDir(), "state.yaml"), "proxy")
		if err != nil {
			t.Fatal(err)
		}
		if resolvedPath != "" {
			t.Fatalf("resolved kubeconfig = %q, want standard loading", resolvedPath)
		}
		if writePath != kubeconfigPath {
			t.Fatalf("kubeconfig write path = %q, want %q", writePath, kubeconfigPath)
		}
	})

	t.Run("explicit path", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Chdir(tempDir)
		resolvedPath, writePath, _, err := resolveAddContextPaths("config", filepath.Join(tempDir, "state.yaml"), "proxy")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(tempDir, "config")
		if resolvedPath != want || writePath != want {
			t.Fatalf("resolved paths = %q, %q, want %q", resolvedPath, writePath, want)
		}
	})
}

func TestResolveAddContextListenAddrRejectsInvalidAddress(t *testing.T) {
	if _, err := resolveAddContextListenAddr("bad-listen"); err == nil {
		t.Fatal("resolveAddContextListenAddr returned nil error")
	}
}

func TestServeStateStopsAfterTTLWithoutRequests(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.test"}`))
	}))
	defer upstream.Close()

	sourcePath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	listenAddr, err := pickAvailableListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	token := "state-token"
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "ttl-proxy",
		SourceKubeconfig: sourcePath,
		Listen:           listenAddr,
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
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
	readyClient, err := newProfileHTTPClient(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(readyClient, profile, 2*time.Second); err != nil {
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

func TestServeStateReplacesProcessWhenStateFileChanges(t *testing.T) {
	alpha := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"target":"alpha"}`))
	}))
	defer alpha.Close()
	beta := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"target":"beta"}`))
	}))
	defer beta.Close()

	sourcePath := writeMainTestKubeconfigWithContexts(t, []mainTestContext{
		{name: "alpha", serverURL: alpha.URL, caData: mainTestServerCAData(alpha)},
		{name: "beta", serverURL: beta.URL, caData: mainTestServerCAData(beta)},
	})
	listenAddr, err := pickAvailableListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "restart-proxy",
		SourceKubeconfig: sourcePath,
		Listen:           listenAddr,
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
		BearerToken:      "state-token",
		ProxyTTL:         "0s",
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
	statePath := filepath.Join(t.TempDir(), "restart-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	replacedStatePath, errCh, replaceErr := startReloadServe(statePath)

	readyClient, err := newProfileHTTPClient(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(readyClient, profile, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if body := getProxyBody(t, profile, "/version"); !strings.Contains(body, `"target":"alpha"`) {
		t.Fatalf("initial proxy body = %s, want alpha target", body)
	}

	profile.Contexts = proxystate.ContextSelection{Names: []string{"beta"}, Primary: "beta"}
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	assertServeProcessReplacement(t, statePath, replacedStatePath, errCh, replaceErr)
}

func TestServeStateReloadsOIDCCredentialsWhenKubeconfigChanges(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"authorization":%q}`, r.Header.Get("Authorization"))
	}))
	defer upstream.Close()

	test := newOIDCReloadTest(t, upstream)
	replacedStatePath, errCh, replaceErr := startReloadServe(test.statePath)
	assertProxyUsesOIDCToken(t, test.profile, test.initialToken)
	replaceOIDCTokenPreservingMetadata(t, test.sourcePath, test.refreshedToken)
	assertServeProcessReplacement(t, test.statePath, replacedStatePath, errCh, replaceErr)
}

func TestRunServeStateReportsLogOpenError(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.test"}`))
	}))
	defer upstream.Close()

	test := newOIDCReloadTest(t, upstream)
	test.profile.LogsEnabled = true
	if err := proxystate.Save(test.statePath, test.profile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(test.statePath+".log", 0o700); err != nil {
		t.Fatal(err)
	}

	err := runServeStateWithReplacer([]string{"--state", test.statePath}, nil, func(string) error {
		t.Fatal("process replacement called after log open error")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), test.statePath+".log") {
		t.Fatalf("serve error = %v, want log path", err)
	}
}

type oidcReloadTest struct {
	profile        *proxystate.Profile
	statePath      string
	sourcePath     string
	initialToken   string
	refreshedToken string
}

func newOIDCReloadTest(t *testing.T, upstream *httptest.Server) oidcReloadTest {
	t.Helper()

	initialToken := validMainTestOIDCToken("initial")
	refreshedToken := validMainTestOIDCToken("renewed")
	sourcePath := writeMainTestKubeconfigWithContexts(t, []mainTestContext{{
		name:      "alpha",
		serverURL: upstream.URL,
		caData:    mainTestServerCAData(upstream),
		authProvider: &clientcmdapi.AuthProviderConfig{
			Name: "oidc",
			Config: map[string]string{
				"idp-issuer-url": "https://issuer.example.test",
				"client-id":      "kubeconfig-proxy-test",
				"id-token":       initialToken,
			},
		},
	}})
	listenAddr, err := pickAvailableListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "oidc-reload-proxy",
		SourceKubeconfig: sourcePath,
		Listen:           listenAddr,
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
		BearerToken:      "state-token",
		ProxyTTL:         "0s",
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
	statePath := filepath.Join(t.TempDir(), "oidc-reload-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}
	return oidcReloadTest{
		profile:        profile,
		statePath:      statePath,
		sourcePath:     sourcePath,
		initialToken:   initialToken,
		refreshedToken: refreshedToken,
	}
}

func startReloadServe(statePath string) (<-chan string, <-chan error, error) {
	replacedStatePath := make(chan string, 1)
	replaceErr := errors.New("test process replaced")
	replace := func(statePath string) error {
		replacedStatePath <- statePath
		return replaceErr
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServeStateWithReplacer([]string{"--state", statePath}, nil, replace)
	}()
	return replacedStatePath, errCh, replaceErr
}

func assertProxyUsesOIDCToken(t *testing.T, profile *proxystate.Profile, token string) {
	t.Helper()

	readyClient, err := newProfileHTTPClient(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(readyClient, profile, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if body := getProxyBody(t, profile, "/version"); !strings.Contains(body, token) {
		t.Fatalf("proxy body = %s, want OIDC token", body)
	}
}

func replaceOIDCTokenPreservingMetadata(t *testing.T, sourcePath, refreshedToken string) {
	t.Helper()

	initialSourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.LoadFromFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	config.AuthInfos["user-alpha"].AuthProvider.Config["id-token"] = refreshedToken
	if err := clientcmd.WriteToFile(*config, sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, initialSourceInfo.ModTime(), initialSourceInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	updatedSourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if updatedSourceInfo.Size() != initialSourceInfo.Size() {
		t.Fatalf("updated source kubeconfig size = %d, want unchanged size %d", updatedSourceInfo.Size(), initialSourceInfo.Size())
	}
}

func assertServeProcessReplacement(t *testing.T, statePath string, replacedStatePath <-chan string, errCh <-chan error, replaceErr error) {
	t.Helper()

	select {
	case gotStatePath := <-replacedStatePath:
		if gotStatePath != statePath {
			t.Fatalf("restarted state path = %q, want %q", gotStatePath, statePath)
		}
	case err := <-errCh:
		t.Fatalf("serve exited before process replacement: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not replace its process after runtime configuration changed")
	}
	if err := <-errCh; !errors.Is(err, replaceErr) {
		t.Fatalf("serve error = %v, want process replacement error", err)
	}
}

func TestServeHTTPRuntimeReloadWaitsForInFlightRequest(t *testing.T) {
	listenAddr, err := pickAvailableListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	tlsCertificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	releaseRequest := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/watch" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		close(requestStarted)
		select {
		case <-r.Context().Done():
			close(requestCanceled)
		case <-releaseRequest:
			_, _ = w.Write([]byte("ok"))
		}
	})
	runtimeChanged := make(chan error, 1)
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- serveHTTP(
			listener,
			handler,
			tlsCertificate,
			0,
			"state-token",
			nil,
			runtimeChanged,
			log.New(io.Discard, "", 0),
		)
	}()

	client, err := newProfileHTTPClient(&proxystate.Profile{TLS: proxystate.TLS{CertPEM: string(certPEM)}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://"+listenAddr+"/watch", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer state-token")
	requestErrCh := make(chan error, 1)
	go func() {
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
		}
		requestErrCh <- requestErr
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("active request did not start")
	}
	runtimeChanged <- errSourceKubeconfigChanged
	select {
	case <-requestCanceled:
		t.Fatal("runtime reload canceled an in-flight request")
	case err := <-serveErrCh:
		t.Fatalf("serveHTTP returned while a request was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	probeRequest, err := http.NewRequest(http.MethodGet, "https://"+listenAddr+"/probe", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	probeRequest.Header.Set("Authorization", "Bearer state-token")
	probeResponse, err := client.Do(probeRequest)
	if err != nil {
		t.Fatalf("proxy stopped accepting requests while reload was pending: %v", err)
	}
	_ = probeResponse.Body.Close()
	if probeResponse.StatusCode != http.StatusOK {
		t.Fatalf("probe status while reload was pending = %d, want %d", probeResponse.StatusCode, http.StatusOK)
	}
	close(releaseRequest)
	if err := <-requestErrCh; err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}
	if err := <-serveErrCh; !errors.Is(err, errSourceKubeconfigChanged) {
		t.Fatalf("serveHTTP error = %v, want source kubeconfig change", err)
	}
}

func TestServeHTTPObservesStateRemovalWhileReloadIsPending(t *testing.T) {
	listener, tlsCertificate := newServeHTTPTestListener(t)
	releaseRequest := make(chan struct{})
	requestStarted := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		_, _ = w.Write([]byte("ok"))
	})
	runtimeChanged := make(chan error)
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- serveHTTP(
			listener,
			handler,
			tlsCertificate,
			0,
			"state-token",
			nil,
			runtimeChanged,
			log.New(io.Discard, "", 0),
		)
	}()

	certificate, err := x509.ParseCertificate(tlsCertificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}}}
	request, err := http.NewRequest(http.MethodGet, "https://"+listener.Addr().String()+"/watch", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer state-token")
	requestErrCh := make(chan error, 1)
	go func() {
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
		}
		requestErrCh <- requestErr
	}()
	<-requestStarted

	runtimeChanged <- errSourceKubeconfigChanged
	stateRemovedErr := stateFileRemovedError("state.yaml")
	cancelSend := make(chan struct{})
	removalAccepted := make(chan bool, 1)
	go func() {
		select {
		case runtimeChanged <- stateRemovedErr:
			removalAccepted <- true
		case <-cancelSend:
			removalAccepted <- false
		}
	}()
	select {
	case accepted := <-removalAccepted:
		if !accepted {
			t.Fatal("state removal send was canceled unexpectedly")
		}
	case <-time.After(time.Second):
		close(cancelSend)
		close(releaseRequest)
		<-requestErrCh
		t.Fatal("serveHTTP stopped observing runtime file errors while reload was pending")
	}
	close(cancelSend)
	close(releaseRequest)
	if err := <-requestErrCh; err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}
	if err := <-serveErrCh; !errors.Is(err, errStateFileRemoved) {
		t.Fatalf("serveHTTP error = %v, want state file removal", err)
	}
}

func TestServeHTTPContinuesAfterRuntimeWatcherCloses(t *testing.T) {
	listener, tlsCertificate := newServeHTTPTestListener(t)
	runtimeChanged := make(chan error)
	close(runtimeChanged)
	stop := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- serveHTTP(
			listener,
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
			tlsCertificate,
			0,
			"state-token",
			stop,
			runtimeChanged,
			log.New(io.Discard, "", 0),
		)
	}()

	time.Sleep(2 * statePollInterval)
	stop <- os.Interrupt
	if err := <-errCh; err != nil {
		t.Fatalf("serveHTTP error = %v, want nil", err)
	}
}

func TestServeHTTPReturnsListenerError(t *testing.T) {
	listener, tlsCertificate := newServeHTTPTestListener(t)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	err := serveHTTP(
		listener,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		tlsCertificate,
		0,
		"state-token",
		make(chan os.Signal),
		nil,
		log.New(io.Discard, "", 0),
	)
	if err == nil {
		t.Fatal("serveHTTP returned nil for a closed listener")
	}
}

func newServeHTTPTestListener(t *testing.T) (net.Listener, tls.Certificate) {
	t.Helper()

	listenAddr, err := pickAvailableListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	tlsCertificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return listener, tlsCertificate
}

func TestReexecServeProcessUsesCurrentExecutableAndState(t *testing.T) {
	wantErr := errors.New("exec failed")
	statePath := filepath.Join(t.TempDir(), "proxy.yaml")
	var gotExecutable string
	var gotArgs []string
	err := reexecServeProcessWithExec(statePath, func(executable string, args []string, _ []string) error {
		gotExecutable = executable
		gotArgs = append([]string(nil), args...)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("reexec error = %v, want injected error", err)
	}
	if gotExecutable == "" {
		t.Fatal("reexec executable is empty")
	}
	if want := []string{gotExecutable, "serve", "--state", statePath}; !slices.Equal(gotArgs, want) {
		t.Fatalf("reexec args = %v, want %v", gotArgs, want)
	}
	if err := reexecServeProcessWithExec(statePath, func(string, []string, []string) error { return nil }); err != nil {
		t.Fatalf("successful reexec = %v, want nil", err)
	}
}

func TestNormalizeServeError(t *testing.T) {
	wantErr := errors.New("serve failed")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "nil", err: nil, want: nil},
		{name: "server closed", err: http.ErrServerClosed, want: nil},
		{name: "serve failure", err: wantErr, want: wantErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeServeError(test.err); !errors.Is(got, test.want) {
				t.Fatalf("normalizeServeError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestWatchRuntimeFilesPrioritizesKubeconfigChange(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.yaml")
	sourcePath := filepath.Join(tempDir, "source.yaml")
	if err := os.WriteFile(statePath, []byte("state-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("source-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateSnapshot, err := readRuntimeFileSnapshot(statePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceSnapshot, err := readRuntimeFileSnapshot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("state-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("source-after"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := watchRuntimeFiles(ctx, statePath, stateSnapshot, []watchedRuntimeFile{{path: sourcePath, snapshot: sourceSnapshot}})
	select {
	case err := <-changed:
		if !errors.Is(err, errSourceKubeconfigChanged) {
			t.Fatalf("runtime file change = %v, want source kubeconfig change", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime file watcher did not detect changes")
	}
}

func TestWatchRuntimeFilesContinuesAfterContentChange(t *testing.T) {
	statePath, sourcePath, stateSnapshot, sourceSnapshot := newRuntimeFileWatcherTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := watchRuntimeFiles(ctx, statePath, stateSnapshot, []watchedRuntimeFile{{path: sourcePath, snapshot: sourceSnapshot}})

	if err := os.WriteFile(sourcePath, []byte("source-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRuntimeFileWatcherEvent(t, changed, errSourceKubeconfigChanged)
	if err := os.WriteFile(statePath, []byte("state-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRuntimeFileWatcherEvent(t, changed, errStateFileChanged)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	assertRuntimeFileWatcherEvent(t, changed, errStateFileRemoved)
}

func TestWatchRuntimeFilesReportsStateReadError(t *testing.T) {
	statePath, sourcePath, stateSnapshot, sourceSnapshot := newRuntimeFileWatcherTest(t)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}

	changed := watchRuntimeFiles(context.Background(), statePath, stateSnapshot, []watchedRuntimeFile{{path: sourcePath, snapshot: sourceSnapshot}})
	assertRuntimeFileWatcherError(t, changed, "read state file")
}

func TestWatchRuntimeFilesDetectsKubeconfigRemoval(t *testing.T) {
	statePath, sourcePath, stateSnapshot, sourceSnapshot := newRuntimeFileWatcherTest(t)
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}

	changed := watchRuntimeFiles(context.Background(), statePath, stateSnapshot, []watchedRuntimeFile{{path: sourcePath, snapshot: sourceSnapshot}})
	assertRuntimeFileWatcherEvent(t, changed, errSourceKubeconfigChanged)
}

func TestWatchRuntimeFilesDetectsNewKubeconfigPrecedenceFile(t *testing.T) {
	statePath, _, stateSnapshot, _ := newRuntimeFileWatcherTest(t)
	kubeconfigPath := filepath.Join(t.TempDir(), "new-kubeconfig")
	kubeconfigFiles, err := snapshotKubeconfigFiles([]string{kubeconfigPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeconfigPath, []byte("new source"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := watchRuntimeFiles(ctx, statePath, stateSnapshot, kubeconfigFiles)
	assertRuntimeFileWatcherEvent(t, changed, errSourceKubeconfigChanged)
}

func TestSnapshotKubeconfigFilesSkipsEmptyAndDuplicatePaths(t *testing.T) {
	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := snapshotKubeconfigFiles([]string{"", kubeconfigPath, kubeconfigPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("watched kubeconfig files = %d, want 1", len(files))
	}
	if files[0].path != kubeconfigPath || !files[0].snapshot.exists {
		t.Fatalf("watched kubeconfig file = %+v, want existing %q", files[0], kubeconfigPath)
	}
}

func TestSnapshotKubeconfigFilesReturnsReadError(t *testing.T) {
	_, err := snapshotKubeconfigFiles([]string{t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "read source kubeconfig") {
		t.Fatalf("snapshotKubeconfigFiles error = %v, want source kubeconfig read error", err)
	}
}

func TestWaitForStableOptionalRuntimeFileSnapshot(t *testing.T) {
	t.Run("file appears", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}

		snapshot, err := waitForStableOptionalRuntimeFileSnapshot(context.Background(), path, runtimeFileSnapshot{})
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.exists {
			t.Fatal("stable optional snapshot does not contain the new file")
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := waitForStableOptionalRuntimeFileSnapshot(ctx, filepath.Join(t.TempDir(), "kubeconfig"), runtimeFileSnapshot{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stable optional snapshot error = %v, want context cancellation", err)
		}
	})

	t.Run("read error", func(t *testing.T) {
		_, err := waitForStableOptionalRuntimeFileSnapshot(context.Background(), t.TempDir(), runtimeFileSnapshot{})
		if err == nil {
			t.Fatal("stable optional snapshot returned nil read error")
		}
	})
}

func TestWatchRuntimeFilesReportsKubeconfigReadError(t *testing.T) {
	statePath, _, stateSnapshot, _ := newRuntimeFileWatcherTest(t)
	kubeconfigPath := t.TempDir()

	changed := watchRuntimeFiles(context.Background(), statePath, stateSnapshot, []watchedRuntimeFile{{path: kubeconfigPath}})
	assertRuntimeFileWatcherError(t, changed, "read source kubeconfig")
}

func TestWatchRuntimeFilesStopsWhenCanceled(t *testing.T) {
	statePath, sourcePath, stateSnapshot, sourceSnapshot := newRuntimeFileWatcherTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	changed := watchRuntimeFiles(ctx, statePath, stateSnapshot, []watchedRuntimeFile{{path: sourcePath, snapshot: sourceSnapshot}})
	cancel()

	select {
	case _, ok := <-changed:
		if ok {
			t.Fatal("runtime file watcher emitted a change after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("runtime file watcher did not stop after cancellation")
	}
}

func TestSendRuntimeFileEventStopsWhenCanceled(t *testing.T) {
	changed := make(chan error, 1)
	changed <- errStateFileChanged
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if sendRuntimeFileEvent(ctx, changed, errSourceKubeconfigChanged) {
		t.Fatal("sendRuntimeFileEvent reported a blocked event after cancellation")
	}
}

func TestWaitForStableRuntimeFileSnapshotReturnsLatestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	partialSnapshot, err := readRuntimeFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}

	stableSnapshot, err := waitForStableRuntimeFileSnapshot(context.Background(), path, partialSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	completeSnapshot, err := readRuntimeFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !stableSnapshot.isEqual(completeSnapshot) {
		t.Fatal("stable runtime file snapshot does not contain the final write")
	}
}

func TestWaitForStableRuntimeFileSnapshotStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitForStableRuntimeFileSnapshot(ctx, filepath.Join(t.TempDir(), "runtime.yaml"), runtimeFileSnapshot{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stable runtime file snapshot error = %v, want context cancellation", err)
	}
}

func TestWaitForStableRuntimeFileSnapshotReportsReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")

	_, err := waitForStableRuntimeFileSnapshot(context.Background(), path, runtimeFileSnapshot{})
	if !os.IsNotExist(err) {
		t.Fatalf("stable runtime file snapshot error = %v, want missing file", err)
	}
}

func TestWatchRuntimeFilesStopsWhenSourceEventCannotBeDelivered(t *testing.T) {
	statePath, sourcePath, stateSnapshot, sourceSnapshot := newRuntimeFileWatcherTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	changed := watchRuntimeFiles(ctx, statePath, stateSnapshot, []watchedRuntimeFile{{path: sourcePath, snapshot: sourceSnapshot}})

	if err := os.WriteFile(sourcePath, []byte("source-after-first-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(statePollInterval + 2*runtimeFileSettleInterval)
	if err := os.WriteFile(sourcePath, []byte("source-after-second-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(statePollInterval + 2*runtimeFileSettleInterval)
	cancel()

	assertRuntimeFileWatcherEvent(t, changed, errSourceKubeconfigChanged)
	assertRuntimeFileWatcherClosed(t, changed)
}

func TestWatchRuntimeFilesStopsWhenStateEventCannotBeDelivered(t *testing.T) {
	statePath, sourcePath, stateSnapshot, sourceSnapshot := newRuntimeFileWatcherTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	changed := watchRuntimeFiles(ctx, statePath, stateSnapshot, []watchedRuntimeFile{{path: sourcePath, snapshot: sourceSnapshot}})

	if err := os.WriteFile(statePath, []byte("state-after-first-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(statePollInterval + 2*runtimeFileSettleInterval)
	if err := os.WriteFile(statePath, []byte("state-after-second-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(statePollInterval + 2*runtimeFileSettleInterval)
	cancel()

	assertRuntimeFileWatcherEvent(t, changed, errStateFileChanged)
	assertRuntimeFileWatcherClosed(t, changed)
}

func newRuntimeFileWatcherTest(t *testing.T) (string, string, runtimeFileSnapshot, runtimeFileSnapshot) {
	t.Helper()

	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.yaml")
	sourcePath := filepath.Join(tempDir, "source.yaml")
	if err := os.WriteFile(statePath, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateSnapshot, err := readRuntimeFileSnapshot(statePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceSnapshot, err := readRuntimeFileSnapshot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	return statePath, sourcePath, stateSnapshot, sourceSnapshot
}

func assertRuntimeFileWatcherError(t *testing.T, changed <-chan error, want string) {
	t.Helper()

	select {
	case err := <-changed:
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("runtime file watcher error = %v, want to contain %q", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("runtime file watcher did not report %q", want)
	}
}

func assertRuntimeFileWatcherEvent(t *testing.T, changed <-chan error, want error) {
	t.Helper()

	select {
	case err := <-changed:
		if !errors.Is(err, want) {
			t.Fatalf("runtime file watcher event = %v, want %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("runtime file watcher did not report %v", want)
	}
}

func assertRuntimeFileWatcherClosed(t *testing.T, changed <-chan error) {
	t.Helper()

	select {
	case _, ok := <-changed:
		if ok {
			t.Fatal("runtime file watcher emitted an unexpected event")
		}
	case <-time.After(time.Second):
		t.Fatal("runtime file watcher did not stop after cancellation")
	}
}

func TestServeStateStopsWhenStateFileDisappears(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.test"}`))
	}))
	defer upstream.Close()

	sourcePath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	listenAddr, err := pickAvailableListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "removed-proxy",
		SourceKubeconfig: sourcePath,
		Listen:           listenAddr,
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
		BearerToken:      "state-token",
		ProxyTTL:         "0s",
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
	statePath := filepath.Join(t.TempDir(), "removed-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runWithArgs([]string{"serve", "--state", statePath}, nil)
	}()
	readyClient, err := newProfileHTTPClient(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(readyClient, profile, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, errStateFileRemoved) {
			t.Fatalf("serve error = %v, want errStateFileRemoved", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop after state file disappeared")
	}
}

func TestReadinessDoesNotRefreshActivityTTL(t *testing.T) {
	nextCalled := atomic.Bool{}
	handler := newActivityHandler(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		nextCalled.Store(true)
	}), "state-token")
	handler.lastActivity.Store(time.Now().Add(-time.Minute).UnixNano())

	req := httptest.NewRequest(http.MethodGet, readinessPath, http.NoBody)
	req.Header.Set("Authorization", "Bearer state-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if nextCalled.Load() {
		t.Fatal("readiness request should not be proxied to the upstream handler")
	}
	if !handler.isIdleFor(time.Second) {
		t.Fatal("readiness request unexpectedly refreshed last activity")
	}
}

func TestReadinessRejectsMissingBearerToken(t *testing.T) {
	nextCalled := atomic.Bool{}
	handler := newActivityHandler(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		nextCalled.Store(true)
	}), "state-token")

	req := httptest.NewRequest(http.MethodGet, readinessPath, http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q, want text/plain", got)
	}
	if got, want := rec.Body.String(), "unauthorized\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if nextCalled.Load() {
		t.Fatal("unauthorized readiness request should not be proxied")
	}
}

func TestActivityHandlerIsBusyWhileRequestInFlight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	handler := newActivityHandler(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}), "state-token")

	req := httptest.NewRequest(http.MethodGet, "/version", http.NoBody)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()
	defer func() {
		close(release)
		<-done
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start request")
	}
	if handler.isIdleFor(0) {
		t.Fatal("handler should not be idle while request is in flight")
	}
}

func TestActivityHandlerDrainsOnlyWhenIdle(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	handler := newActivityHandler(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}), "state-token")

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/version", http.NoBody))
		close(done)
	}()
	<-started
	if handler.beginDrainIfIdle() {
		t.Fatal("activity handler began draining with a request in flight")
	}
	close(release)
	<-done
	if !handler.beginDrainIfIdle() {
		t.Fatal("idle activity handler did not begin draining")
	}

	proxied := httptest.NewRecorder()
	handler.ServeHTTP(proxied, httptest.NewRequest(http.MethodGet, "/version", http.NoBody))
	if proxied.Code != http.StatusServiceUnavailable {
		t.Fatalf("proxied status while draining = %d, want %d", proxied.Code, http.StatusServiceUnavailable)
	}

	readinessRequest := httptest.NewRequest(http.MethodGet, readinessPath, http.NoBody)
	readinessRequest.Header.Set("Authorization", "Bearer state-token")
	readiness := httptest.NewRecorder()
	handler.ServeHTTP(readiness, readinessRequest)
	if readiness.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status while draining = %d, want %d", readiness.Code, http.StatusServiceUnavailable)
	}
}

func TestActivityHandlerCoalescesIdleNotifications(t *testing.T) {
	handler := newActivityHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "state-token")

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/version", http.NoBody))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/version", http.NoBody))

	select {
	case <-handler.becameIdle:
	default:
		t.Fatal("activity handler did not report becoming idle")
	}
	select {
	case <-handler.becameIdle:
		t.Fatal("activity handler queued duplicate idle notifications")
	default:
	}
}

func TestRunCredentialWritesExecCredentialWhenProxyIsReady(t *testing.T) {
	token := "state-token"
	ready := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readinessPath {
			t.Fatalf("path = %q, want readiness path", r.URL.Path)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
			t.Fatalf("authorization = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer ready.Close()

	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "credential-proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           strings.TrimPrefix(ready.URL, "https://"),
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
		BearerToken:      token,
		ProxyTTL:         "10m",
		TLS: proxystate.TLS{
			CertPEM: string(mainTestServerCAData(ready)),
			KeyPEM:  "unused by credential command",
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout: "30s",
			Retries:        0,
			RetryBackoff:   "1ms",
		},
	}
	statePath := filepath.Join(t.TempDir(), "credential-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	output := captureStdout(t, func() error {
		return runCredential([]string{"--state", statePath})
	})

	var payload map[string]any
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatal(err)
	}
	status, ok := payload["status"].(map[string]any)
	if !ok {
		t.Fatalf("status payload = %#v", payload["status"])
	}
	if status["token"] != token {
		t.Fatalf("token = %q, want %q", status["token"], token)
	}
	if status["expirationTimestamp"] == "" {
		t.Fatal("expirationTimestamp is missing")
	}
	if _, err := os.Stat(statePath + ".lock"); err != nil {
		t.Fatalf("lock file stat error = %v", err)
	}
}

func TestRunCredentialStartsDetachedServeWhenProxyIsNotReady(t *testing.T) {
	token := "state-token"
	var readinessChecks atomic.Int32
	ready := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if readinessChecks.Add(1) == 1 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer ready.Close()

	var startedStatePath atomic.Value
	commandFactory := func(executable, statePath string) *exec.Cmd {
		startedStatePath.Store(statePath)
		cmd := exec.Command(executable, "-test.run=TestDetachedServeHelperProcess")
		cmd.Env = append(os.Environ(), "KCP_TEST_DETACHED_HELPER=1", "KCP_TEST_DETACHED_HELPER_DELAY=500ms")
		return cmd
	}

	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "credential-proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           strings.TrimPrefix(ready.URL, "https://"),
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
		BearerToken:      token,
		ProxyTTL:         "0",
		TLS: proxystate.TLS{
			CertPEM: string(mainTestServerCAData(ready)),
			KeyPEM:  "unused by credential command",
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout: "30s",
			Retries:        0,
			RetryBackoff:   "1ms",
		},
	}
	statePath := filepath.Join(t.TempDir(), "credential-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	output := captureStdout(t, func() error {
		return runCredentialWithCommandFactory([]string{"--state", statePath}, commandFactory)
	})
	if !strings.Contains(output, `"token":"state-token"`) {
		t.Fatalf("credential output = %s, want token", output)
	}
	if got := startedStatePath.Load(); got != statePath {
		t.Fatalf("detached serve state path = %v, want %s", got, statePath)
	}
	if got := readinessChecks.Load(); got < 2 {
		t.Fatalf("readiness checks = %d, want initial failure and retry", got)
	}
}

func TestRunWithArgsDispatchesCredential(t *testing.T) {
	token := "state-token"
	ready := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer ready.Close()

	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "credential-proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           strings.TrimPrefix(ready.URL, "https://"),
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
		BearerToken:      token,
		ProxyTTL:         "0",
		TLS: proxystate.TLS{
			CertPEM: string(mainTestServerCAData(ready)),
			KeyPEM:  "unused by credential command",
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout: "30s",
			Retries:        0,
			RetryBackoff:   "1ms",
		},
	}
	statePath := filepath.Join(t.TempDir(), "credential-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	output := captureStdout(t, func() error {
		return runWithArgs([]string{"credential", "--state", statePath}, nil)
	})
	if !strings.Contains(output, `"token":"state-token"`) {
		t.Fatalf("credential output = %s, want token", output)
	}
}

func TestRunCredentialRejectsInvalidFlag(t *testing.T) {
	err := runCredential([]string{"--unknown"})
	if err == nil {
		t.Fatal("runCredential returned nil error")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("error = %q, want invalid flag error", err.Error())
	}
}

func TestRunCredentialRequiresStateFlag(t *testing.T) {
	err := runCredential(nil)
	if err == nil {
		t.Fatal("runCredential returned nil error")
	}
	if err.Error() != "--state is required" {
		t.Fatalf("error = %q, want --state is required", err.Error())
	}
}

func TestStartDetachedServeStartsHelperProcessAndWritesLogs(t *testing.T) {
	commandFactory := func(executable, statePath string) *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=TestDetachedServeHelperProcess")
		cmd.Env = append(os.Environ(), "KCP_TEST_DETACHED_HELPER=1", "KCP_TEST_DETACHED_STATE="+statePath)
		return cmd
	}

	statePath := filepath.Join(t.TempDir(), "proxy.yaml")
	if _, err := startDetachedServe(statePath, true, commandFactory); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(statePath + ".log"); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestDetachedServeHelperProcess(t *testing.T) {
	if os.Getenv("KCP_TEST_DETACHED_HELPER") != "1" {
		t.Skip("helper process")
	}
	if os.Getenv("KCP_TEST_DETACHED_HELPER_FAIL") == "1" {
		t.Fatal("requested detached helper failure")
	}
	if delay := os.Getenv("KCP_TEST_DETACHED_HELPER_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(duration)
	}
}

func TestRunCredentialReturnsLoadAndClientErrors(t *testing.T) {
	t.Run("missing state file", func(t *testing.T) {
		err := runCredential([]string{"--state", filepath.Join(t.TempDir(), "missing.yaml")})
		if err == nil {
			t.Fatal("runCredential returned nil error")
		}
		if !os.IsNotExist(err) {
			t.Fatalf("error = %v, want not-exist error", err)
		}
	})

	t.Run("invalid proxy ttl", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "proxy.yaml")
		if err := os.WriteFile(statePath, []byte(`version: 1
name: proxy
sourceKubeconfig: /tmp/kubeconfig
listen: 127.0.0.1:9443
contexts:
  names: [alpha]
  primary: alpha
bearerToken: token
proxyTTL: nope
tls:
  certPEM: cert
  keyPEM: key
options:
  requestTimeout: 1s
  retries: 0
  retryBackoff: 1ms
`), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runCredential([]string{"--state", statePath})
		if err == nil {
			t.Fatal("runCredential returned nil error")
		}
		if !strings.Contains(err.Error(), "parse proxyTTL") {
			t.Fatalf("error = %q, want proxyTTL parse error", err.Error())
		}
	})

	t.Run("invalid tls cert", func(t *testing.T) {
		profile := validMainTestProfile()
		statePath := filepath.Join(t.TempDir(), "proxy.yaml")
		if err := proxystate.Save(statePath, profile); err != nil {
			t.Fatal(err)
		}
		err := runCredential([]string{"--state", statePath})
		if err == nil {
			t.Fatal("runCredential returned nil error")
		}
		if !strings.Contains(err.Error(), "state TLS certificate is not valid PEM") {
			t.Fatalf("error = %q, want invalid TLS cert error", err.Error())
		}
	})
}

func TestCheckReadyReturnsStatusError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := server.Client()
	profile := &proxystate.Profile{
		Listen:      strings.TrimPrefix(server.URL, "https://"),
		BearerToken: "token",
	}
	err := checkReady(client, profile)
	if err == nil {
		t.Fatal("checkReady returned nil error")
	}
	if !strings.Contains(err.Error(), "readiness status 503") {
		t.Fatalf("error = %q, want readiness status 503", err.Error())
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	profile := &proxystate.Profile{
		Listen:      strings.TrimPrefix(server.URL, "https://"),
		BearerToken: "token",
	}
	err := waitReady(server.Client(), profile, 25*time.Millisecond)
	if err == nil {
		t.Fatal("waitReady returned nil error")
	}
	if !strings.Contains(err.Error(), "proxy did not become ready") {
		t.Fatalf("error = %q, want timeout readiness error", err.Error())
	}
}

func TestRunCredentialWaitsForSlowDetachedServe(t *testing.T) {
	token := "state-token"
	var readinessChecks atomic.Int32
	ready := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if readinessChecks.Add(1) < 3 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer ready.Close()

	commandFactory := func(executable, _ string) *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=TestDetachedServeHelperProcess")
		cmd.Env = append(os.Environ(), "KCP_TEST_DETACHED_HELPER=1", "KCP_TEST_DETACHED_HELPER_DELAY=500ms")
		return cmd
	}
	statePath := writeCredentialTestState(t, ready, token)

	output := captureStdout(t, func() error {
		return runCredentialWithCommandFactoryAndTimeout([]string{"--state", statePath}, commandFactory, time.Second)
	})
	if !strings.Contains(output, `"token":"state-token"`) {
		t.Fatalf("credential output = %s, want token", output)
	}
	if got := readinessChecks.Load(); got < 3 {
		t.Fatalf("readiness checks = %d, want startup retries", got)
	}
}

func TestRunCredentialReturnsWhenDetachedServeExitsBeforeReadiness(t *testing.T) {
	ready := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer ready.Close()

	commandFactory := func(executable, _ string) *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=TestDetachedServeHelperProcess")
		cmd.Env = append(os.Environ(), "KCP_TEST_DETACHED_HELPER=1", "KCP_TEST_DETACHED_HELPER_FAIL=1")
		return cmd
	}
	statePath := writeCredentialTestState(t, ready, "state-token")
	err := runCredentialWithCommandFactoryAndTimeout([]string{"--state", statePath}, commandFactory, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "serve exited before readiness") {
		t.Fatalf("credential error = %v, want early serve exit", err)
	}
}

func writeCredentialTestState(t *testing.T, server *httptest.Server, token string) string {
	t.Helper()
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "credential-proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           strings.TrimPrefix(server.URL, "https://"),
		Contexts:         proxystate.ContextSelection{Names: []string{"alpha"}, Primary: "alpha"},
		BearerToken:      token,
		ProxyTTL:         "0",
		TLS: proxystate.TLS{
			CertPEM: string(mainTestServerCAData(server)),
			KeyPEM:  "unused by credential command",
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout: "30s",
			Retries:        0,
			RetryBackoff:   "1ms",
		},
	}
	statePath := filepath.Join(t.TempDir(), "credential-proxy.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}
	return statePath
}

func TestProfileHTTPClientRejectsInvalidCertificate(t *testing.T) {
	_, err := newProfileHTTPClient(&proxystate.Profile{TLS: proxystate.TLS{CertPEM: "not pem"}})
	if err == nil {
		t.Fatal("newProfileHTTPClient returned nil error")
	}
	if !strings.Contains(err.Error(), "state TLS certificate is not valid PEM") {
		t.Fatalf("error = %q, want invalid PEM error", err.Error())
	}
}

func TestProfileHTTPClientRequiresTLS12(t *testing.T) {
	certPEM, _, err := generateTLSCertificate("127.0.0.1:9443")
	if err != nil {
		t.Fatal(err)
	}

	client, err := newProfileHTTPClient(&proxystate.Profile{TLS: proxystate.TLS{CertPEM: string(certPEM)}})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	if got := transport.TLSClientConfig.MinVersion; got != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want %d", got, tls.VersionTLS12)
	}
}

func TestWrapMissingStateErr(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "missing.yaml")
	err := wrapMissingStateErr(os.ErrNotExist, statePath)
	if !errors.Is(err, errStateFileRemoved) {
		t.Fatalf("wrapped missing error = %v, want errStateFileRemoved", err)
	}

	other := errors.New("boom")
	if got := wrapMissingStateErr(other, statePath); got != other {
		t.Fatalf("wrapped non-missing error = %v, want original", got)
	}
}

func TestTTLCheckIntervalBounds(t *testing.T) {
	tests := []struct {
		ttl  time.Duration
		want time.Duration
	}{
		{ttl: 0, want: time.Second},
		{ttl: time.Millisecond, want: 10 * time.Millisecond},
		{ttl: 400 * time.Millisecond, want: 100 * time.Millisecond},
		{ttl: 10 * time.Second, want: time.Second},
	}

	for _, tt := range tests {
		if got := calculateTTLCheckInterval(tt.ttl); got != tt.want {
			t.Fatalf("calculateTTLCheckInterval(%s) = %s, want %s", tt.ttl, got, tt.want)
		}
	}
}

func TestRunServeStateRejectsInvalidArgs(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantErrContains string
	}{
		{
			name:            "missing state",
			args:            []string{"serve"},
			wantErrContains: "--state is required",
		},
		{
			name:            "invalid flag",
			args:            []string{"serve", "--unknown"},
			wantErrContains: "flag provided but not defined",
		},
		{
			name:            "missing state file",
			args:            []string{"serve", "--state", filepath.Join(t.TempDir(), "missing.yaml")},
			wantErrContains: "state file removed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithArgs(tt.args, nil)
			if err == nil {
				t.Fatal("runWithArgs returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

func TestLoadServeRuntimeRejectsInvalidTLSKeyPair(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.test"}`))
	}))
	defer upstream.Close()

	sourcePath := writeMainTestKubeconfig(t, upstream.URL, mainTestServerCAData(upstream))
	profile := validMainTestProfile()
	profile.SourceKubeconfig = sourcePath
	profile.TLS.CertPEM = "-----BEGIN CERTIFICATE-----\nnot-base64\n-----END CERTIFICATE-----\n"
	profile.TLS.KeyPEM = "-----BEGIN EC PRIVATE KEY-----\nnot-base64\n-----END EC PRIVATE KEY-----\n"
	statePath := filepath.Join(t.TempDir(), "proxy.yaml")
	if err := os.WriteFile(statePath, []byte(`version: 1
name: proxy
sourceKubeconfig: `+sourcePath+`
listen: 127.0.0.1:9443
contexts:
  names: [alpha]
  primary: alpha
bearerToken: token
proxyTTL: 10m
tls:
  certPEM: |
    -----BEGIN CERTIFICATE-----
    not-base64
    -----END CERTIFICATE-----
  keyPEM: |
    -----BEGIN EC PRIVATE KEY-----
    not-base64
    -----END EC PRIVATE KEY-----
options:
  requestTimeout: 1s
  retries: 0
  retryBackoff: 1ms
`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadServeRuntime(statePath)
	if err == nil {
		t.Fatal("loadServeRuntime returned nil error")
	}
	if !strings.Contains(err.Error(), "load TLS key pair from state") {
		t.Fatalf("error = %q, want TLS key pair error", err.Error())
	}
}

func TestLoadServeRuntimeReturnsKubeconfigErrors(t *testing.T) {
	tests := []struct {
		name            string
		writeSource     bool
		sourceContents  string
		wantErrContains string
	}{
		{
			name:            "missing source",
			wantErrContains: "read source kubeconfig",
		},
		{
			name:            "invalid source",
			writeSource:     true,
			sourceContents:  "contexts: [",
			wantErrContains: "error loading config file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tempDir := t.TempDir()
			sourcePath := filepath.Join(tempDir, "source.yaml")
			if test.writeSource {
				if err := os.WriteFile(sourcePath, []byte(test.sourceContents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			profile := validMainTestProfile()
			profile.SourceKubeconfig = sourcePath
			statePath := filepath.Join(tempDir, "state.yaml")
			if err := proxystate.Save(statePath, profile); err != nil {
				t.Fatal(err)
			}

			_, _, err := loadServeRuntime(statePath)
			if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("loadServeRuntime error = %v, want to contain %q", err, test.wantErrContains)
			}
		})
	}
}

func TestLoadServeRuntimeReturnsDefaultKubeconfigError(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))
	profile := validMainTestProfile()
	profile.SourceKubeconfig = ""
	statePath := filepath.Join(t.TempDir(), "state.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadServeRuntime(statePath)
	if err == nil || !strings.Contains(err.Error(), "standard Kubernetes loading rules") {
		t.Fatalf("loadServeRuntime error = %v, want default kubeconfig loading error", err)
	}
}

func TestLoadServeRuntimeRejectsMissingSelectedContext(t *testing.T) {
	sourcePath := writeMainTestKubeconfig(t, "https://cluster.example.test", nil)
	profile := validMainTestProfile()
	profile.SourceKubeconfig = sourcePath
	profile.Contexts = proxystate.ContextSelection{Names: []string{"missing"}, Primary: "missing"}
	statePath := filepath.Join(t.TempDir(), "state.yaml")
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadServeRuntime(statePath)
	if err == nil || !strings.Contains(err.Error(), `context "missing" not found`) {
		t.Fatalf("loadServeRuntime error = %v, want missing context", err)
	}
}

func TestLoadServeRuntimeResolvesNewRegexpMatches(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.test"}`))
	}))
	defer upstream.Close()

	kubeconfigPath := writeMainTestKubeconfigWithContexts(t, []mainTestContext{{
		name: "prod-a", serverURL: upstream.URL, caData: mainTestServerCAData(upstream),
	}})
	statePath := filepath.Join(t.TempDir(), "regexp-proxy.yaml")
	if err := runWithArgs([]string{
		"add-context", "proxy",
		"--kubeconfig", kubeconfigPath,
		"--state", statePath,
		"--listen", "127.0.0.1:0",
		"--context-regexp", "^prod-",
	}, nil); err != nil {
		t.Fatal(err)
	}

	runtime, _, err := loadServeRuntime(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetNames(runtime.targets); !slices.Equal(got, []string{"prod-a"}) {
		t.Fatalf("initial targets = %v, want [prod-a]", got)
	}

	config, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	config.Clusters["cluster-prod-b"] = &clientcmdapi.Cluster{
		Server: upstream.URL, CertificateAuthorityData: mainTestServerCAData(upstream),
	}
	config.AuthInfos["user-prod-b"] = &clientcmdapi.AuthInfo{Token: "source-token"}
	config.Contexts["prod-b"] = &clientcmdapi.Context{Cluster: "cluster-prod-b", AuthInfo: "user-prod-b"}
	if err := clientcmd.WriteToFile(*config, kubeconfigPath); err != nil {
		t.Fatal(err)
	}

	runtime, _, err = loadServeRuntime(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetNames(runtime.targets); !slices.Equal(got, []string{"prod-a", "prod-b"}) {
		t.Fatalf("targets after kubeconfig update = %v, want [prod-a prod-b]", got)
	}
}

func targetNames(targets []proxy.Target) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	return names
}

func TestServeRuntimeReturnsListenError(t *testing.T) {
	runtime := &serveRuntimeConfig{profile: &proxystate.Profile{Listen: "127.0.0.1:-1"}}
	err := serveRuntime("state.yaml", runtime, runtimeFileSnapshot{}, nil, log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("serveRuntime returned nil for invalid listen address")
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
	expiration := expirationForExecCredential(now, 10*time.Minute)
	if expiration == nil {
		t.Fatal("expiration is nil")
	}
	if got, want := expiration.Sub(now), 9*time.Minute+50*time.Second; got != want {
		t.Fatalf("valid duration = %s, want %s", got, want)
	}
	if expiration := expirationForExecCredential(now, 0); expiration != nil {
		t.Fatalf("expiration = %v, want nil when proxyTTL is disabled", expiration)
	}
	if expiration := expirationForExecCredential(now, time.Nanosecond); expiration == nil || expiration.Sub(now) != time.Nanosecond {
		t.Fatalf("tiny ttl expiration = %v, want now + 1ns", expiration)
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
	caData, keyData, err := generateTLSCertificate("127.0.0.1:9443")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(caData, keyData)
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

func TestGenerateTLSCertificateIncludesDNSAndLoopbackFallbackSANs(t *testing.T) {
	_, _, err := generateTLSCertificate("localhost:9443")
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, keyPEM, err := generateTLSCertificate(":9443")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(parsed.IPAddresses, func(ip net.IP) bool {
		return ip.Equal(net.ParseIP("127.0.0.1"))
	}) {
		t.Fatalf("certificate IP SANs = %v, want fallback 127.0.0.1", parsed.IPAddresses)
	}
}

func mustDefaultStatePath(t *testing.T, contextName string) string {
	t.Helper()
	path, err := resolveDefaultStatePath(contextName)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func writeMainTestKubeconfig(t *testing.T, serverURL string, caData []byte) string {
	t.Helper()

	return writeMainTestKubeconfigWithContexts(t, []mainTestContext{
		{name: "alpha", serverURL: serverURL, caData: caData},
	})
}

type mainTestContext struct {
	name         string
	serverURL    string
	caData       []byte
	authProvider *clientcmdapi.AuthProviderConfig
}

func validMainTestProfile() *proxystate.Profile {
	return &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           "127.0.0.1:9443",
		Contexts: proxystate.ContextSelection{
			Names:   []string{"alpha"},
			Primary: "alpha",
		},
		BearerToken: "token",
		ProxyTTL:    "10m",
		TLS: proxystate.TLS{
			CertPEM: "cert",
			KeyPEM:  "key",
		},
		Options: proxystate.ProxyOptions{
			RequestTimeout: "1s",
			Retries:        0,
			RetryBackoff:   "1ms",
		},
	}
}

func writeMainTestKubeconfigWithContexts(t *testing.T, contexts []mainTestContext) string {
	t.Helper()

	currentContext := ""
	if len(contexts) > 0 {
		currentContext = contexts[0].name
	}
	return writeMainTestKubeconfigWithContextsAndCurrent(t, contexts, currentContext)
}

func writeMainTestKubeconfigWithContextsAndCurrent(t *testing.T, contexts []mainTestContext, currentContext string) string {
	t.Helper()

	config := clientcmdapi.NewConfig()
	for _, context := range contexts {
		clusterName := "cluster-" + context.name
		userName := "user-" + context.name
		config.Clusters[clusterName] = &clientcmdapi.Cluster{
			Server:                   context.serverURL,
			CertificateAuthorityData: context.caData,
		}
		config.AuthInfos[userName] = &clientcmdapi.AuthInfo{
			AuthProvider: context.authProvider,
		}
		if context.authProvider == nil {
			config.AuthInfos[userName].Token = "source-token"
		}
		config.Contexts[context.name] = &clientcmdapi.Context{
			Cluster:   clusterName,
			AuthInfo:  userName,
			Namespace: "test-ns",
		}
	}
	config.CurrentContext = currentContext

	path := filepath.Join(t.TempDir(), "source.yaml")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func validMainTestOIDCToken(subject string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"sub":%q}`, time.Now().Add(time.Hour).Unix(), subject)))
	return header + "." + payload + "."
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()

	oldStdout := os.Stdout
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writePipe
	defer func() {
		os.Stdout = oldStdout
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- fn()
		_ = writePipe.Close()
	}()

	output, readErr := io.ReadAll(readPipe)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func getProxyBody(t *testing.T, profile *proxystate.Profile, path string) string {
	t.Helper()

	body, err := tryGetProxyBody(profile, path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func tryGetProxyBody(profile *proxystate.Profile, path string) (string, error) {
	client, err := newProfileHTTPClient(profile)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, "https://"+profile.Listen+path, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+profile.BearerToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("proxy status %d: %s", resp.StatusCode, data)
	}
	return string(data), nil
}

func mainTestServerCAData(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}
