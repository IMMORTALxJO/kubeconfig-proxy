package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
)

type detachedServeCommandFactory func(executable, statePath string) *exec.Cmd

func newDetachedServeCommand(executable, statePath string) *exec.Cmd {
	return exec.Command(executable, "serve", "--state", statePath) // #nosec G204 -- the CLI starts its own executable with an explicit local state path.
}

func runCredential(args []string) error {
	return runCredentialWithCommandFactory(args, newDetachedServeCommand)
}

func runCredentialWithCommandFactory(args []string, commandFactory detachedServeCommandFactory) error {
	return runCredentialWithCommandFactoryAndTimeout(args, commandFactory, credentialStartupTimeout)
}

func runCredentialWithCommandFactoryAndTimeout(args []string, commandFactory detachedServeCommandFactory, startupTimeout time.Duration) error {
	flags := flag.NewFlagSet("kubeconfig-proxy credential", flag.ContinueOnError)
	statePath := flags.String("state", "", "state file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *statePath == "" {
		return fmt.Errorf("--state is required")
	}

	unlock, err := lockState(*statePath)
	if err != nil {
		return err
	}
	defer unlock()

	runtime, err := proxystate.LoadRuntime(*statePath)
	if err != nil {
		return err
	}
	profile := runtime.Profile
	client, err := newProfileHTTPClient(profile)
	if err != nil {
		return err
	}
	if checkReady(client, profile) != nil {
		serveExited, err := startDetachedServe(*statePath, profile.LogsEnabled, commandFactory)
		if err != nil {
			return err
		}
		if err := waitReadyWithServeExit(client, profile, startupTimeout, serveExited); err != nil {
			return err
		}
	}

	return writeExecCredential(os.Stdout, profile.BearerToken, expirationForExecCredential(time.Now(), runtime.ProxyTTL))
}

func lockState(statePath string) (func(), error) {
	lockPath := statePath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- lock path is derived from the explicit local state path.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func waitReady(client *http.Client, profile *proxystate.Profile, timeout time.Duration) error {
	return waitReadyWithServeExit(client, profile, timeout, nil)
}

func waitReadyWithServeExit(client *http.Client, profile *proxystate.Profile, timeout time.Duration, serveExited <-chan error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := checkReady(client, profile); err == nil {
			return nil
		} else {
			lastErr = err
		}
		retryDelay := 100 * time.Millisecond
		if remaining := time.Until(deadline); remaining < retryDelay {
			retryDelay = remaining
		}
		select {
		case err := <-serveExited:
			if err != nil {
				return fmt.Errorf("serve exited before readiness: %w", err)
			}
			return fmt.Errorf("serve exited before readiness")
		case <-time.After(retryDelay):
		}
	}
	return fmt.Errorf("proxy did not become ready at https://%s: %w", profile.Listen, lastErr)
}

func checkReady(client *http.Client, profile *proxystate.Profile) error {
	req, err := http.NewRequest(http.MethodGet, "https://"+profile.Listen+readinessPath, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+profile.BearerToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness status %d", resp.StatusCode)
	}
	return nil
}

func newProfileHTTPClient(profile *proxystate.Profile) (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(profile.TLS.CertPEM)) {
		return nil, fmt.Errorf("state TLS certificate is not valid PEM")
	}
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		}},
	}, nil
}

func startDetachedServe(statePath string, logsEnabled bool, commandFactory detachedServeCommandFactory) (<-chan error, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	nullFile, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer nullFile.Close()

	stdout := io.Writer(nullFile)
	stderr := io.Writer(nullFile)
	if logsEnabled {
		logPath := statePath + ".log"
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- log path is derived from the explicit local state path.
		if err != nil {
			return nil, err
		}
		defer logFile.Close()
		stdout = logFile
		stderr = logFile
	}

	cmd := commandFactory(executable, statePath)
	cmd.Stdin = nullFile
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
	}()
	return exited, nil
}

func writeExecCredential(w io.Writer, token string, expiration *time.Time) error {
	status := map[string]any{
		"token": token,
	}
	if expiration != nil {
		status["expirationTimestamp"] = expiration.Format(time.RFC3339)
	}
	credential := map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1",
		"kind":       "ExecCredential",
		"status":     status,
	}
	encoder := json.NewEncoder(w)
	return encoder.Encode(credential)
}

func expirationForExecCredential(now time.Time, proxyTTL time.Duration) *time.Time {
	if proxyTTL <= 0 {
		return nil
	}
	skew := proxyTTL / 10
	if skew > 10*time.Second {
		skew = 10 * time.Second
	}
	if skew <= 0 {
		skew = proxyTTL / 2
	}
	validFor := proxyTTL - skew
	if validFor <= 0 {
		validFor = proxyTTL / 2
	}
	expiration := now.Add(validFor).UTC()
	return &expiration
}
