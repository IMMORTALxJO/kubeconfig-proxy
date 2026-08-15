package state

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const Version = 1

type temporaryFile interface {
	Name() string
	Write([]byte) (int, error)
	Chmod(os.FileMode) error
	Close() error
}

var createTemporaryFile = func(dir, pattern string) (temporaryFile, error) {
	return os.CreateTemp(dir, pattern)
}

type Profile struct {
	Version          int              `json:"version"`
	Name             string           `json:"name"`
	SourceKubeconfig string           `json:"sourceKubeconfig"`
	Listen           string           `json:"listen"`
	Contexts         ContextSelection `json:"contexts"`
	BearerToken      string           `json:"bearerToken"`
	ProxyTTL         string           `json:"proxyTTL"`
	LogsEnabled      bool             `json:"logsEnabled"`
	TLS              TLS              `json:"tls"`
	Options          ProxyOptions     `json:"options"`
}

type ContextSelection struct {
	Regexp  string   `json:"regexp,omitempty"`
	Names   []string `json:"names,omitempty"`
	Primary string   `json:"primary,omitempty"`
}

type TLS struct {
	CertPEM string `json:"certPEM"`
	KeyPEM  string `json:"keyPEM"`
}

type ProxyOptions struct {
	RequestTimeout   string `json:"requestTimeout"`
	Retries          int    `json:"retries"`
	RetryBackoff     string `json:"retryBackoff"`
	HelmReleaseProxy bool   `json:"helmReleaseProxy"`
	ReadOnly         bool   `json:"readOnly"`
}

type RuntimeProfile struct {
	Profile        *Profile
	ProxyTTL       time.Duration
	RequestTimeout time.Duration
	RetryBackoff   time.Duration
}

func LoadRuntime(path string) (*RuntimeProfile, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- state path is an explicit local CLI input or a path recorded in the generated kubeconfig.
	if err != nil {
		return nil, err
	}
	var profile Profile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	runtime, err := profile.validatedRuntime()
	if err != nil {
		return nil, err
	}
	return runtime, nil
}

func Save(path string, profile *Profile) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(profile) // #nosec G117 -- state intentionally stores proxy secrets and is written with 0600 permissions.
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := createTemporaryFile(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (p *Profile) Validate() error {
	_, err := p.validatedRuntime()
	return err
}

func (p *Profile) validatedRuntime() (*RuntimeProfile, error) {
	if p.Version != Version {
		return nil, fmt.Errorf("unsupported state version %d", p.Version)
	}
	if err := p.validateRequiredFields(); err != nil {
		return nil, err
	}
	proxyTTL, err := parseDuration("proxyTTL", p.ProxyTTL)
	if err != nil {
		return nil, err
	}
	requestTimeout, err := parseDuration("options.requestTimeout", p.Options.RequestTimeout)
	if err != nil {
		return nil, err
	}
	retryBackoff, err := parseDuration("options.retryBackoff", p.Options.RetryBackoff)
	if err != nil {
		return nil, err
	}
	if err := validateNonNegativeDuration("proxyTTL", proxyTTL); err != nil {
		return nil, err
	}
	if err := validateNonNegativeDuration("options.requestTimeout", requestTimeout); err != nil {
		return nil, err
	}
	if p.Options.Retries < 0 {
		return nil, fmt.Errorf("options.retries must be greater than or equal to 0")
	}
	if err := validateNonNegativeDuration("options.retryBackoff", retryBackoff); err != nil {
		return nil, err
	}
	return &RuntimeProfile{
		Profile:        p,
		ProxyTTL:       proxyTTL,
		RequestTimeout: requestTimeout,
		RetryBackoff:   retryBackoff,
	}, nil
}

func (p *Profile) validateRequiredFields() error {
	required := []struct {
		value string
		name  string
	}{
		{p.Name, "state name"},
		{p.SourceKubeconfig, "state sourceKubeconfig"},
		{p.Listen, "state listen"},
		{p.BearerToken, "state bearerToken"},
		{p.TLS.CertPEM, "state tls.certPEM"},
		{p.TLS.KeyPEM, "state tls.keyPEM"},
	}
	for _, field := range required {
		if field.value == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	contextRegexp := strings.TrimSpace(p.Contexts.Regexp)
	if len(p.Contexts.Names) == 0 && contextRegexp == "" {
		return fmt.Errorf("state contexts are required")
	}
	var compiledRegexp *regexp.Regexp
	if contextRegexp != "" {
		var err error
		compiledRegexp, err = regexp.Compile(p.Contexts.Regexp)
		if err != nil {
			return fmt.Errorf("parse contexts.regexp: %w", err)
		}
	}
	seenContexts := make(map[string]struct{}, len(p.Contexts.Names))
	for _, contextName := range p.Contexts.Names {
		if contextName == "" {
			return fmt.Errorf("state context name is required")
		}
		if _, ok := seenContexts[contextName]; ok {
			return fmt.Errorf("state context %q is configured more than once", contextName)
		}
		seenContexts[contextName] = struct{}{}
	}
	if p.Contexts.Primary == "" {
		return nil
	}
	if _, ok := seenContexts[p.Contexts.Primary]; ok {
		return nil
	}
	if compiledRegexp == nil || !compiledRegexp.MatchString(p.Contexts.Primary) {
		return fmt.Errorf("state contexts.primary %q is not selected by contexts", p.Contexts.Primary)
	}
	return nil
}

func validateNonNegativeDuration(name string, duration time.Duration) error {
	if duration < 0 {
		return fmt.Errorf("%s must be greater than or equal to 0", name)
	}
	return nil
}

func parseDuration(name, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return duration, nil
}
