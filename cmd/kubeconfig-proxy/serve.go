package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/upstream"
)

const shutdownTimeout = 5 * time.Second

func runServeState(args []string, stop <-chan os.Signal) error {
	flags := flag.NewFlagSet("kubeconfig-proxy serve", flag.ContinueOnError)
	statePath := flags.String("state", "", "state file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *statePath == "" {
		return errors.New("--state is required")
	}

	runtime, snapshot, err := loadServeRuntime(*statePath)
	if err != nil {
		return err
	}
	for {
		logger, closeLogger, err := newServeLogger(*statePath, runtime.profile.LogsEnabled)
		if err != nil {
			return err
		}
		restart, serveErr := serveRuntime(*statePath, runtime, snapshot, stop, logger)
		closeErr := closeLogger()
		if serveErr == nil {
			serveErr = closeErr
		}
		if serveErr != nil {
			return serveErr
		}
		if !restart {
			return nil
		}

		runtime, snapshot, err = loadServeRuntime(*statePath)
		if err != nil {
			return err
		}
	}
}

type serveRuntimeConfig struct {
	profile        *proxystate.Profile
	requestTimeout time.Duration
	retryBackoff   time.Duration
	proxyTTL       time.Duration
	targets        []proxy.Target
	primary        proxy.Target
	handler        http.Handler
	tlsCertificate tls.Certificate
}

func loadServeRuntime(statePath string) (*serveRuntimeConfig, stateFileSnapshot, error) {
	snapshot, err := readStateFileSnapshot(statePath)
	if err != nil {
		return nil, stateFileSnapshot{}, wrapMissingStateErr(err, statePath)
	}

	stateRuntime, err := proxystate.LoadRuntime(statePath)
	if err != nil {
		return nil, stateFileSnapshot{}, wrapMissingStateErr(err, statePath)
	}
	profile := stateRuntime.Profile

	source, err := kubeconfig.LoadSource(profile.SourceKubeconfig)
	if err != nil {
		return nil, stateFileSnapshot{}, err
	}
	targets, primary, err := upstream.LoadTargets(source, profile.Contexts, profile.PrimaryContext)
	if err != nil {
		return nil, stateFileSnapshot{}, err
	}
	handler, err := proxy.NewWithOptions(targets, primary, proxy.Options{
		RequestTimeout:   stateRuntime.RequestTimeout,
		Retries:          profile.Options.Retries,
		RetryBackoff:     stateRuntime.RetryBackoff,
		BearerToken:      profile.BearerToken,
		HelmReleaseProxy: profile.Options.HelmReleaseProxy,
		ReadOnly:         profile.Options.ReadOnly,
	})
	if err != nil {
		return nil, stateFileSnapshot{}, err
	}

	tlsCertificate, err := tls.X509KeyPair([]byte(profile.TLS.CertPEM), []byte(profile.TLS.KeyPEM))
	if err != nil {
		return nil, stateFileSnapshot{}, fmt.Errorf("load TLS key pair from state: %w", err)
	}

	return &serveRuntimeConfig{
		profile:        profile,
		requestTimeout: stateRuntime.RequestTimeout,
		retryBackoff:   stateRuntime.RetryBackoff,
		proxyTTL:       stateRuntime.ProxyTTL,
		targets:        targets,
		primary:        primary,
		handler:        handler,
		tlsCertificate: tlsCertificate,
	}, snapshot, nil
}

func serveRuntime(statePath string, runtime *serveRuntimeConfig, snapshot stateFileSnapshot, stop <-chan os.Signal, logger *log.Logger) (bool, error) {
	listener, err := net.Listen("tcp", runtime.profile.Listen)
	if err != nil {
		return false, err
	}
	defer listener.Close()

	logger.Printf("state file:       %s", statePath)
	logger.Printf("listen:           https://%s", listener.Addr().String())
	logger.Printf("targets:          %s", upstream.Names(runtime.targets))
	logger.Printf("primary target:   %s", runtime.primary.Name)
	logger.Printf("proxy ttl:        %s", formatDurationForLog(runtime.proxyTTL))
	logger.Printf("request timeout: %s", formatDurationForLog(runtime.requestTimeout))
	logger.Printf("retries:         %d", runtime.profile.Options.Retries)
	logger.Printf("retry backoff:   %s", runtime.retryBackoff)
	logger.Printf("read only:       %t", runtime.profile.Options.ReadOnly)

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	stateChanged := watchStateFile(watchCtx, statePath, snapshot)

	err = serveHTTP(listener, runtime.handler, runtime.tlsCertificate, runtime.proxyTTL, runtime.profile.BearerToken, stop, stateChanged, logger)
	if errors.Is(err, errStateFileChanged) {
		logger.Printf("state file changed, restarting serve")
		return true, nil
	}
	return false, err
}

func serveHTTP(listener net.Listener, handler http.Handler, tlsCertificate tls.Certificate, proxyTTL time.Duration, bearerToken string, stop <-chan os.Signal, stateChanged <-chan error, logger *log.Logger) error {
	activityHandler := newActivityHandler(handler, bearerToken)
	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           activityHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		tlsListener := tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{tlsCertificate},
			MinVersion:   tls.VersionTLS12,
		})
		errCh <- server.Serve(tlsListener)
	}()

	var ttlCh <-chan time.Time
	var ticker *time.Ticker
	if proxyTTL > 0 {
		ticker = time.NewTicker(calculateTTLCheckInterval(proxyTTL))
		defer ticker.Stop()
		ttlCh = ticker.C
	}

	if stop == nil {
		signalStop := make(chan os.Signal, 1)
		signal.Notify(signalStop, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signalStop)
		stop = signalStop
	}

	for {
		select {
		case <-stop:
			logger.Printf("shutting down")
			return shutdownServer(server)
		case err, ok := <-stateChanged:
			if !ok {
				stateChanged = nil
				continue
			}
			if err != nil {
				logger.Printf("shutting down after state file error: %v", err)
			}
			if shutdownErr := shutdownServer(server); shutdownErr != nil {
				return shutdownErr
			}
			if err != nil {
				return err
			}
			return errStateFileChanged
		case <-ttlCh:
			if activityHandler.isIdleFor(proxyTTL) {
				logger.Printf("shutting down after %s without active requests", proxyTTL)
				return shutdownServer(server)
			}
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	}
}

func wrapMissingStateErr(err error, statePath string) error {
	if os.IsNotExist(err) {
		return stateFileRemovedError(statePath)
	}
	return err
}

func shutdownServer(server *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return server.Shutdown(ctx)
}

func newServeLogger(statePath string, enabled bool) (*log.Logger, func() error, error) {
	if !enabled {
		return log.New(io.Discard, "", log.LstdFlags), func() error { return nil }, nil
	}
	logFile, err := os.OpenFile(statePath+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- log path is derived from the explicit local state path.
	if err != nil {
		return nil, nil, err
	}
	return log.New(logFile, "", log.LstdFlags), logFile.Close, nil
}

func calculateTTLCheckInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return time.Second
	}
	interval := ttl / 4
	if interval < 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	if interval > time.Second {
		return time.Second
	}
	return interval
}
