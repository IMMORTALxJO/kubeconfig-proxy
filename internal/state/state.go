package state

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"sigs.k8s.io/yaml"
)

const Version = 1

type Profile struct {
	Version          int          `json:"version"`
	Name             string       `json:"name"`
	SourceKubeconfig string       `json:"sourceKubeconfig"`
	Listen           string       `json:"listen"`
	Contexts         []string     `json:"contexts"`
	PrimaryContext   string       `json:"primaryContext"`
	BearerToken      string       `json:"bearerToken"`
	ProxyTTL         string       `json:"proxyTTL"`
	TLS              TLS          `json:"tls"`
	Options          ProxyOptions `json:"options"`
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
}

func Load(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var profile Profile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return &profile, nil
}

func Save(path string, profile *Profile) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(profile)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
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
	if p.Version != Version {
		return fmt.Errorf("unsupported state version %d", p.Version)
	}
	if p.Name == "" {
		return fmt.Errorf("state name is required")
	}
	if p.SourceKubeconfig == "" {
		return fmt.Errorf("state sourceKubeconfig is required")
	}
	if p.Listen == "" {
		return fmt.Errorf("state listen is required")
	}
	if len(p.Contexts) == 0 {
		return fmt.Errorf("state contexts are required")
	}
	if p.PrimaryContext == "" {
		return fmt.Errorf("state primaryContext is required")
	}
	if p.BearerToken == "" {
		return fmt.Errorf("state bearerToken is required")
	}
	if p.TLS.CertPEM == "" {
		return fmt.Errorf("state tls.certPEM is required")
	}
	if p.TLS.KeyPEM == "" {
		return fmt.Errorf("state tls.keyPEM is required")
	}
	proxyTTL, err := p.ProxyTTLDuration()
	if err != nil {
		return err
	}
	if proxyTTL < 0 {
		return fmt.Errorf("proxyTTL must be greater than or equal to 0")
	}
	requestTimeout, err := p.RequestTimeoutDuration()
	if err != nil {
		return err
	}
	if requestTimeout < 0 {
		return fmt.Errorf("options.requestTimeout must be greater than or equal to 0")
	}
	if p.Options.Retries < 0 {
		return fmt.Errorf("options.retries must be greater than or equal to 0")
	}
	retryBackoff, err := p.RetryBackoffDuration()
	if err != nil {
		return err
	}
	if retryBackoff < 0 {
		return fmt.Errorf("options.retryBackoff must be greater than or equal to 0")
	}
	return nil
}

func (p *Profile) ProxyTTLDuration() (time.Duration, error) {
	return parseDuration("proxyTTL", p.ProxyTTL)
}

func (p *Profile) RequestTimeoutDuration() (time.Duration, error) {
	return parseDuration("options.requestTimeout", p.Options.RequestTimeout)
}

func (p *Profile) RetryBackoffDuration() (time.Duration, error) {
	return parseDuration("options.retryBackoff", p.Options.RetryBackoff)
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
