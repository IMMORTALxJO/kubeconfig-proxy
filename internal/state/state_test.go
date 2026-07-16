package state

import (
	"strings"
	"testing"
)

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

func validTestProfile() *Profile {
	return &Profile{
		Version:          Version,
		Name:             "test-proxy",
		SourceKubeconfig: "/tmp/kubeconfig",
		Listen:           "127.0.0.1:9443",
		Contexts:         []string{"kind-test"},
		PrimaryContext:   "kind-test",
		BearerToken:      "token",
		ProxyTTL:         "10m",
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
