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

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
	proxystate "github.com/IMMORTALxJO/kubeconfig-proxy/internal/state"
)

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
	if !runtime.profile.LogsEnabled {
		restoreLogs := discardLogs()
		defer restoreLogs()
	}

	for {
		restart, err := serveRuntime(*statePath, runtime, snapshot, stop)
		if !restart {
			return err
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
		if os.IsNotExist(err) {
			return nil, stateFileSnapshot{}, stateFileRemovedError(statePath)
		}
		return nil, stateFileSnapshot{}, err
	}

	profile, err := proxystate.Load(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, stateFileSnapshot{}, stateFileRemovedError(statePath)
		}
		return nil, stateFileSnapshot{}, err
	}
	requestTimeout, err := profile.RequestTimeoutDuration()
	if err != nil {
		return nil, stateFileSnapshot{}, err
	}
	retryBackoff, err := profile.RetryBackoffDuration()
	if err != nil {
		return nil, stateFileSnapshot{}, err
	}
	proxyTTL, err := profile.ProxyTTLDuration()
	if err != nil {
		return nil, stateFileSnapshot{}, err
	}

	targets, primary, err := proxy.LoadTargets(profile.SourceKubeconfig, profile.Contexts, profile.PrimaryContext)
	if err != nil {
		return nil, stateFileSnapshot{}, err
	}
	handler, err := proxy.NewWithOptions(targets, primary, proxy.Options{
		RequestTimeout:   requestTimeout,
		Retries:          profile.Options.Retries,
		RetryBackoff:     retryBackoff,
		BearerToken:      profile.BearerToken,
		HelmReleaseProxy: profile.Options.HelmReleaseProxy,
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
		requestTimeout: requestTimeout,
		retryBackoff:   retryBackoff,
		proxyTTL:       proxyTTL,
		targets:        targets,
		primary:        primary,
		handler:        handler,
		tlsCertificate: tlsCertificate,
	}, snapshot, nil
}

func serveRuntime(statePath string, runtime *serveRuntimeConfig, snapshot stateFileSnapshot, stop <-chan os.Signal) (bool, error) {
	listener, err := proxy.Listen(runtime.profile.Listen)
	if err != nil {
		return false, err
	}
	defer listener.Close()

	log.Printf("state file:       %s", statePath)
	log.Printf("listen:           https://%s", listener.Addr().String())
	log.Printf("targets:          %s", proxy.TargetNames(runtime.targets))
	log.Printf("primary target:   %s", runtime.primary.Name)
	log.Printf("proxy ttl:        %s", durationLogValue(runtime.proxyTTL))
	log.Printf("request timeout: %s", durationLogValue(runtime.requestTimeout))
	log.Printf("retries:         %d", runtime.profile.Options.Retries)
	log.Printf("retry backoff:   %s", runtime.retryBackoff)

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	stateChanged := watchStateFile(watchCtx, statePath, snapshot)

	err = serveHTTP(listener, runtime.handler, runtime.tlsCertificate, runtime.proxyTTL, runtime.profile.BearerToken, stop, stateChanged)
	if errors.Is(err, errStateFileChanged) {
		log.Printf("state file changed, restarting serve")
		return true, nil
	}
	return false, err
}

func serveHTTP(listener net.Listener, handler http.Handler, tlsCertificate tls.Certificate, proxyTTL time.Duration, bearerToken string, stop <-chan os.Signal, stateChanged <-chan error) error {
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

	ttlCh := (<-chan time.Time)(nil)
	ticker := (*time.Ticker)(nil)
	if proxyTTL > 0 {
		ticker = time.NewTicker(ttlCheckInterval(proxyTTL))
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
			log.Printf("shutting down")
			return shutdownServer(server)
		case err, ok := <-stateChanged:
			if !ok {
				stateChanged = nil
				continue
			}
			if err != nil {
				log.Printf("shutting down after state file error: %v", err)
			}
			if shutdownErr := shutdownServer(server); shutdownErr != nil {
				return shutdownErr
			}
			if err != nil {
				return err
			}
			return errStateFileChanged
		case <-ttlCh:
			if activityHandler.idleFor(proxyTTL) {
				log.Printf("shutting down after %s without active requests", proxyTTL)
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

func shutdownServer(server *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), proxy.ShutdownTimeout)
	defer cancel()
	return server.Shutdown(ctx)
}

func discardLogs() func() {
	output := log.Writer()
	flags := log.Flags()
	prefix := log.Prefix()
	log.SetOutput(io.Discard)
	return func() {
		log.SetOutput(output)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}
}

func ttlCheckInterval(ttl time.Duration) time.Duration {
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
