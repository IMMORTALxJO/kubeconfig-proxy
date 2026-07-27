package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
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
	if got := durationLogValue(0); got != "disabled" {
		t.Fatalf("durationLogValue(0) = %q, want disabled", got)
	}
	if got := durationLogValue(2 * time.Second); got != "2s" {
		t.Fatalf("durationLogValue(2s) = %q, want 2s", got)
	}
	if got := appendUniqueStrings([]string{"alpha"}, "", "alpha", "beta"); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("appendUniqueStrings = %v, want [alpha beta]", got)
	}
	if got := safeFileName("prod/blue:west_1"); got != "prod_blue_west_1" {
		t.Fatalf("safeFileName = %q, want prod_blue_west_1", got)
	}
	if got := safeFileName("Prod.Blue-1"); got != "Prod.Blue-1" {
		t.Fatalf("safeFileName uppercase/dot/dash = %q, want Prod.Blue-1", got)
	}
	if got := defaultStatePath("prod/blue"); !strings.HasSuffix(got, filepath.Join(".kube", "kubeconfig-proxy", "prod_blue.yaml")) {
		t.Fatalf("defaultStatePath = %q, want kubeconfig-proxy prod_blue suffix", got)
	}
}

func TestResolveAddContextTargets(t *testing.T) {
	kubeconfigPath := writeMainTestKubeconfigWithContextsAndCurrent(t, []mainTestContext{
		{name: "alpha", serverURL: "https://alpha.example.test"},
		{name: "beta", serverURL: "https://beta.example.test"},
		{name: "prod-west", serverURL: "https://prod-west.example.test"},
		{name: "proxy", serverURL: "https://proxy.example.test"},
	}, "beta")

	tests := []struct {
		name            string
		selected        []string
		contextRegexp   string
		primary         string
		wantContexts    []string
		wantPrimary     string
		wantErrContains string
	}{
		{
			name:         "explicit contexts keep selected order and current primary",
			selected:     []string{"prod-west", "beta"},
			wantContexts: []string{"prod-west", "beta"},
			wantPrimary:  "beta",
		},
		{
			name:          "regexp selects sorted contexts excluding proxy context",
			contextRegexp: "^prod|alpha$",
			wantContexts:  []string{"alpha", "prod-west"},
			wantPrimary:   "alpha",
		},
		{
			name:         "explicit primary overrides current context",
			selected:     []string{"alpha", "prod-west"},
			primary:      "prod-west",
			wantContexts: []string{"alpha", "prod-west"},
			wantPrimary:  "prod-west",
		},
		{
			name:            "contexts and regexp conflict",
			selected:        []string{"alpha"},
			contextRegexp:   "beta",
			wantErrContains: "--contexts and --context-regexp are mutually exclusive",
		},
		{
			name:            "invalid regexp",
			contextRegexp:   "[",
			wantErrContains: "missing closing ]",
		},
		{
			name:            "missing selected context",
			selected:        []string{"missing"},
			wantErrContains: `context "missing" not found`,
		},
		{
			name:            "selected proxy context",
			selected:        []string{"proxy"},
			wantErrContains: `source contexts must not include proxy context "proxy"`,
		},
		{
			name:            "primary outside selected contexts",
			selected:        []string{"alpha"},
			primary:         "beta",
			wantErrContains: `primary context "beta" is not included`,
		},
		{
			name:            "empty regexp selection",
			contextRegexp:   "^missing$",
			wantErrContains: "no source contexts selected",
		},
		{
			name:         "default selects sorted contexts excluding proxy context",
			wantContexts: []string{"alpha", "beta", "prod-west"},
			wantPrimary:  "beta",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contexts, primary, err := resolveAddContextTargets(kubeconfigPath, "proxy", tt.selected, tt.contextRegexp, tt.primary)
			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatal("resolveAddContextTargets returned nil error")
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

func TestResolveAddContextTargetsReturnsLoadError(t *testing.T) {
	_, _, err := resolveAddContextTargets(filepath.Join(t.TempDir(), "missing.yaml"), "proxy", nil, "", "")
	if err == nil {
		t.Fatal("resolveAddContextTargets returned nil error")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("error = %v, want not-exist error", err)
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
	if profile.LogsEnabled {
		t.Fatal("profile logsEnabled = true, want false by default")
	}
	if profile.Options.ReadOnly {
		t.Fatal("profile readOnly = true, want false by default")
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

	profile, err := proxystate.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
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

	profile, err := proxystate.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
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

	statePath := defaultStatePath("flag-first-proxy")
	profile, err := proxystate.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
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
	statePath := defaultStatePath("missing-proxy")
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
	readyClient, err := profileHTTPClient(profile)
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

func TestServeStateRestartsWhenStateFileChanges(t *testing.T) {
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
	_, certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "restart-proxy",
		SourceKubeconfig: sourcePath,
		Listen:           listenAddr,
		Contexts:         []string{"alpha"},
		PrimaryContext:   "alpha",
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

	stop := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runWithArgs([]string{"serve", "--state", statePath}, stop)
	}()
	defer stopServeAndWait(t, stop, errCh)

	readyClient, err := profileHTTPClient(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(readyClient, profile, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if body := getProxyBody(t, profile, "/version"); !strings.Contains(body, `"target":"alpha"`) {
		t.Fatalf("initial proxy body = %s, want alpha target", body)
	}

	profile.Contexts = []string{"beta"}
	profile.PrimaryContext = "beta"
	if err := proxystate.Save(statePath, profile); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("serve did not restart with updated state")
		}
		body, err := tryGetProxyBody(profile, "/version")
		if err == nil && strings.Contains(body, `"target":"beta"`) {
			return
		}
		select {
		case err := <-errCh:
			t.Fatalf("serve exited while waiting for restart: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
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
	_, certPEM, keyPEM, err := generateTLSCertificate(listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "removed-proxy",
		SourceKubeconfig: sourcePath,
		Listen:           listenAddr,
		Contexts:         []string{"alpha"},
		PrimaryContext:   "alpha",
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
	readyClient, err := profileHTTPClient(profile)
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

func TestReadinessRefreshesActivityTTL(t *testing.T) {
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
	if handler.idleFor(time.Second) {
		t.Fatal("readiness request did not refresh last activity")
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
	if handler.idleFor(0) {
		t.Fatal("handler should not be idle while request is in flight")
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
		Contexts:         []string{"alpha"},
		PrimaryContext:   "alpha",
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
	if os.Getenv("KCP_TEST_CREDENTIAL_DETACHED_HELPER") == "1" {
		os.Exit(0)
	}

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

	oldCommand := newDetachedServeCommand
	var startedStatePath atomic.Value
	newDetachedServeCommand = func(executable, statePath string) *exec.Cmd {
		startedStatePath.Store(statePath)
		cmd := exec.Command(executable, "-test.run=TestRunCredentialStartsDetachedServeWhenProxyIsNotReady")
		cmd.Env = append(os.Environ(), "KCP_TEST_CREDENTIAL_DETACHED_HELPER=1")
		return cmd
	}
	t.Cleanup(func() {
		newDetachedServeCommand = oldCommand
	})

	profile := &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "credential-proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           strings.TrimPrefix(ready.URL, "https://"),
		Contexts:         []string{"alpha"},
		PrimaryContext:   "alpha",
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
		return runCredential([]string{"--state", statePath})
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
		Contexts:         []string{"alpha"},
		PrimaryContext:   "alpha",
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
	if os.Getenv("KCP_TEST_DETACHED_SERVE_HELPER") == "1" {
		os.Exit(0)
	}

	oldCommand := newDetachedServeCommand
	newDetachedServeCommand = func(executable, statePath string) *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=TestStartDetachedServeStartsHelperProcessAndWritesLogs")
		cmd.Env = append(os.Environ(), "KCP_TEST_DETACHED_SERVE_HELPER=1", "KCP_TEST_DETACHED_STATE="+statePath)
		return cmd
	}
	t.Cleanup(func() {
		newDetachedServeCommand = oldCommand
	})

	statePath := filepath.Join(t.TempDir(), "proxy.yaml")
	if err := startDetachedServe(statePath, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(statePath + ".log"); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v, want 0600", info.Mode().Perm())
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
contexts: [alpha]
primaryContext: alpha
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

func TestProfileHTTPClientRejectsInvalidCertificate(t *testing.T) {
	_, err := profileHTTPClient(&proxystate.Profile{TLS: proxystate.TLS{CertPEM: "not pem"}})
	if err == nil {
		t.Fatal("profileHTTPClient returned nil error")
	}
	if !strings.Contains(err.Error(), "state TLS certificate is not valid PEM") {
		t.Fatalf("error = %q, want invalid PEM error", err.Error())
	}
}

func TestProfileHTTPClientRequiresTLS12(t *testing.T) {
	_, certPEM, _, err := generateTLSCertificate("127.0.0.1:9443")
	if err != nil {
		t.Fatal(err)
	}

	client, err := profileHTTPClient(&proxystate.Profile{TLS: proxystate.TLS{CertPEM: string(certPEM)}})
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
		if got := ttlCheckInterval(tt.ttl); got != tt.want {
			t.Fatalf("ttlCheckInterval(%s) = %s, want %s", tt.ttl, got, tt.want)
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
contexts: [alpha]
primaryContext: alpha
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
	if expiration := execCredentialExpiration(now, time.Nanosecond); expiration == nil || expiration.Sub(now) != time.Nanosecond {
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

func TestGenerateTLSCertificateIncludesDNSAndLoopbackFallbackSANs(t *testing.T) {
	_, _, _, err := generateTLSCertificate("localhost:9443")
	if err != nil {
		t.Fatal(err)
	}
	certificate, _, _, err := generateTLSCertificate(":9443")
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

func writeMainTestKubeconfig(t *testing.T, serverURL string, caData []byte) string {
	t.Helper()

	return writeMainTestKubeconfigWithContexts(t, []mainTestContext{
		{name: "alpha", serverURL: serverURL, caData: caData},
	})
}

type mainTestContext struct {
	name      string
	serverURL string
	caData    []byte
}

func validMainTestProfile() *proxystate.Profile {
	return &proxystate.Profile{
		Version:          proxystate.Version,
		Name:             "proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           "127.0.0.1:9443",
		Contexts:         []string{"alpha"},
		PrimaryContext:   "alpha",
		BearerToken:      "token",
		ProxyTTL:         "10m",
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
		config.AuthInfos[userName] = &clientcmdapi.AuthInfo{Token: "source-token"}
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
	client, err := profileHTTPClient(profile)
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

func stopServeAndWait(t *testing.T, stop chan<- os.Signal, errCh <-chan error) {
	t.Helper()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve exited unexpectedly: %v", err)
		}
		return
	default:
	}
	stop <- os.Interrupt
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve stop error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func mainTestServerCAData(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}
