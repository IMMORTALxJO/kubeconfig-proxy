package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type failingTemporaryFile struct{ writeErr, chmodErr, closeErr error }

func (failingTemporaryFile) Name() string                { return filepath.Join(os.TempDir(), "state-test.tmp") }
func (f failingTemporaryFile) Write([]byte) (int, error) { return 0, f.writeErr }
func (f failingTemporaryFile) Chmod(os.FileMode) error   { return f.chmodErr }
func (f failingTemporaryFile) Close() error              { return f.closeErr }

func TestSaveAndLoadRuntimeRoundTripWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "proxy.yaml")
	profile := validTestProfile()
	profile.LogsEnabled = true
	profile.Options.HelmReleaseProxy = true
	profile.Options.ReadOnly = true

	if err := Save(path, profile); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", got)
	}

	runtime, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded := runtime.Profile
	if loaded.Name != profile.Name {
		t.Fatalf("loaded name = %q, want %q", loaded.Name, profile.Name)
	}
	if loaded.TLS.CertPEM != profile.TLS.CertPEM || loaded.TLS.KeyPEM != profile.TLS.KeyPEM {
		t.Fatalf("loaded TLS = %#v, want %#v", loaded.TLS, profile.TLS)
	}
	if !loaded.LogsEnabled || !loaded.Options.HelmReleaseProxy || !loaded.Options.ReadOnly {
		t.Fatalf("loaded options = %#v logs=%t, want enabled flags", loaded.Options, loaded.LogsEnabled)
	}
}

func TestSaveOmitsEmptySourceKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.yaml")
	profile := validTestProfile()
	profile.SourceKubeconfig = ""

	if err := Save(path, profile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sourceKubeconfig:") {
		t.Fatalf("state contains an empty sourceKubeconfig key:\n%s", data)
	}
	runtime, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Profile.SourceKubeconfig != "" {
		t.Fatalf("loaded source kubeconfig = %q, want empty", runtime.Profile.SourceKubeconfig)
	}
}

func TestSaveAndLoadRuntimeUsesSourceKubeconfigKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.yaml")
	profile := validTestProfile()

	if err := Save(path, profile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sourceKubeconfig: /tmp/kubeconfig") {
		t.Fatalf("state does not contain sourceKubeconfig:\n%s", data)
	}
	if strings.Contains(string(data), "\nkubeconfig:") {
		t.Fatalf("state contains obsolete kubeconfig key:\n%s", data)
	}

	runtime, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Profile.SourceKubeconfig != profile.SourceKubeconfig {
		t.Fatalf("loaded source kubeconfig = %q, want %q", runtime.Profile.SourceKubeconfig, profile.SourceKubeconfig)
	}
}

func TestLoadRuntimeRejectsInvalidStateFiles(t *testing.T) {
	tests := []struct {
		name            string
		data            string
		wantErrContains string
	}{
		{
			name:            "invalid yaml",
			data:            "version: [",
			wantErrContains: "parse state file",
		},
		{
			name: "invalid version",
			data: `version: 2
name: test
sourceKubeconfig: /tmp/kubeconfig
listen: 127.0.0.1:9443
contexts:
  names: [alpha]
  primary: alpha
bearerToken: token
proxyTTL: 10m
tls:
  certPEM: cert
  keyPEM: key
options:
  requestTimeout: 30s
  retries: 1
  retryBackoff: 100ms
`,
			wantErrContains: "unsupported state version 2",
		},
		{
			name: "invalid duration",
			data: `version: 1
name: test
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
  requestTimeout: 30s
  retries: 1
  retryBackoff: 100ms
`,
			wantErrContains: "parse proxyTTL",
		},
		{
			name: "legacy contexts list",
			data: `version: 1
name: test
sourceKubeconfig: /tmp/kubeconfig
listen: 127.0.0.1:9443
contexts: [alpha]
bearerToken: token
tls:
  certPEM: cert
  keyPEM: key
`,
			wantErrContains: "parse state file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxy.yaml")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := LoadRuntime(path)
			if err == nil {
				t.Fatal("LoadRuntime returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

func TestLoadRuntimeReturnsReadError(t *testing.T) {
	_, err := LoadRuntime(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("LoadRuntime returned nil error")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("LoadRuntime error = %v, want not-exist error", err)
	}
}

func TestSaveRejectsInvalidProfile(t *testing.T) {
	profile := validTestProfile()
	profile.Name = ""

	err := Save(filepath.Join(t.TempDir(), "proxy.yaml"), profile)
	if err == nil {
		t.Fatal("Save returned nil error")
	}
	if !strings.Contains(err.Error(), "state name is required") {
		t.Fatalf("error = %q, want missing name validation", err.Error())
	}
}

func TestSaveReturnsDirectoryCreationError(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parentFile, "proxy.yaml")
	err := Save(path, validTestProfile())
	if err == nil {
		t.Fatal("Save returned nil error")
	}
}

func TestSaveReturnsTemporaryFileErrors(t *testing.T) {
	tests := []struct {
		name      string
		file      temporaryFile
		createErr error
	}{
		{name: "create", createErr: errors.New("create")},
		{name: "write", file: failingTemporaryFile{writeErr: errors.New("write")}},
		{name: "chmod", file: failingTemporaryFile{chmodErr: errors.New("chmod")}},
		{name: "close", file: failingTemporaryFile{closeErr: errors.New("close")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := createTemporaryFile
			createTemporaryFile = func(string, string) (temporaryFile, error) { return tt.file, tt.createErr }
			t.Cleanup(func() { createTemporaryFile = original })
			if err := Save(filepath.Join(t.TempDir(), "state.yaml"), validTestProfile()); err == nil || !strings.Contains(err.Error(), tt.name) {
				t.Fatalf("Save error = %v", err)
			}
		})
	}
}

func TestValidateRejectsNegativeRuntimeOptions(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*Profile)
		wantErrContains string
	}{
		{
			name: "negative proxy ttl",
			mutate: func(profile *Profile) {
				profile.ProxyTTL = "-1s"
			},
			wantErrContains: "proxyTTL must be greater than or equal to 0",
		},
		{
			name: "negative request timeout",
			mutate: func(profile *Profile) {
				profile.Options.RequestTimeout = "-1s"
			},
			wantErrContains: "options.requestTimeout must be greater than or equal to 0",
		},
		{
			name: "negative retries",
			mutate: func(profile *Profile) {
				profile.Options.Retries = -1
			},
			wantErrContains: "options.retries must be greater than or equal to 0",
		},
		{
			name: "negative retry backoff",
			mutate: func(profile *Profile) {
				profile.Options.RetryBackoff = "-1s"
			},
			wantErrContains: "options.retryBackoff must be greater than or equal to 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := validTestProfile()
			tt.mutate(profile)
			err := profile.Validate()
			if err == nil {
				t.Fatal("Validate returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

func TestValidateRejectsMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*Profile)
		wantErrContains string
	}{
		{
			name: "name",
			mutate: func(profile *Profile) {
				profile.Name = ""
			},
			wantErrContains: "state name is required",
		},
		{
			name: "listen",
			mutate: func(profile *Profile) {
				profile.Listen = ""
			},
			wantErrContains: "state listen is required",
		},
		{
			name: "contexts",
			mutate: func(profile *Profile) {
				profile.Contexts = ContextSelection{}
			},
			wantErrContains: "state contexts are required",
		},
		{
			name: "bearer token",
			mutate: func(profile *Profile) {
				profile.BearerToken = ""
			},
			wantErrContains: "state bearerToken is required",
		},
		{
			name: "cert pem",
			mutate: func(profile *Profile) {
				profile.TLS.CertPEM = ""
			},
			wantErrContains: "state tls.certPEM is required",
		},
		{
			name: "key pem",
			mutate: func(profile *Profile) {
				profile.TLS.KeyPEM = ""
			},
			wantErrContains: "state tls.keyPEM is required",
		},
		{
			name: "duplicate contexts",
			mutate: func(profile *Profile) {
				profile.Contexts.Names = []string{"alpha", "alpha"}
			},
			wantErrContains: `state context "alpha" is configured more than once`,
		},
		{
			name: "primary context outside contexts",
			mutate: func(profile *Profile) {
				profile.Contexts.Primary = "beta"
			},
			wantErrContains: `state contexts.primary "beta" is not selected by contexts`,
		},
		{
			name: "invalid context regexp",
			mutate: func(profile *Profile) {
				profile.Contexts.Regexp = "["
			},
			wantErrContains: "parse contexts.regexp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := validTestProfile()
			tt.mutate(profile)
			err := profile.Validate()
			if err == nil {
				t.Fatal("Validate returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

func TestValidateAcceptsCombinedContextSelectorsAndOptionalPrimary(t *testing.T) {
	profile := validTestProfile()
	profile.Contexts = ContextSelection{
		Regexp: "^prod-",
		Names:  []string{"shared"},
	}

	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAcceptsZeroRuntimeOptions(t *testing.T) {
	profile := validTestProfile()
	profile.ProxyTTL = "0"
	profile.Options.RequestTimeout = "0"
	profile.Options.Retries = 0
	profile.Options.RetryBackoff = "0"

	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRuntimeParsesDurationsOnce(t *testing.T) {
	profile := validTestProfile()
	profile.ProxyTTL = "2m"
	profile.Options.RequestTimeout = "3s"
	profile.Options.RetryBackoff = "150ms"
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := Save(path, profile); err != nil {
		t.Fatal(err)
	}
	runtime, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ProxyTTL != 2*time.Minute {
		t.Fatalf("ProxyTTL = %s, want 2m", runtime.ProxyTTL)
	}
	if runtime.RequestTimeout != 3*time.Second {
		t.Fatalf("RequestTimeout = %s, want 3s", runtime.RequestTimeout)
	}
	if runtime.RetryBackoff != 150*time.Millisecond {
		t.Fatalf("RetryBackoff = %s, want 150ms", runtime.RetryBackoff)
	}
}

func validTestProfile() *Profile {
	return &Profile{
		Version:          Version,
		Name:             "test-proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           "127.0.0.1:9443",
		Contexts: ContextSelection{
			Names:   []string{"kind-test"},
			Primary: "kind-test",
		},
		BearerToken: "token",
		ProxyTTL:    "10m",
		TLS: TLS{
			CertPEM: "cert",
			KeyPEM:  "key",
		},
		Options: ProxyOptions{
			RequestTimeout: "30s",
			Retries:        5,
			RetryBackoff:   "200ms",
		},
	}
}
