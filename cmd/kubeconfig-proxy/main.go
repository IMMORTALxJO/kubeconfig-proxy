package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/kubeconfig"
	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run() error {
	return runWithArgs(os.Args[1:], nil)
}

func runWithArgs(args []string, stop <-chan os.Signal) error {
	flags := flag.NewFlagSet("kubeconfig-proxy", flag.ContinueOnError)
	var (
		kubeconfigPath = flags.String("kubeconfig", "", "source kubeconfig path; defaults to standard kubeconfig loading rules")
		outputPath     = flags.String("output", filepath.Join(mustHomeDir(), ".kube", "config.proxy"), "proxy kubeconfig output path")
		listenAddr     = flags.String("listen", "127.0.0.1:0", "proxy listen address")
		contextsCSV    = flags.String("contexts", "", "comma-separated kubeconfig contexts to include; defaults to all contexts")
		primaryContext = flags.String("primary-context", "", "context used for single-cluster operations; defaults to current context")
		requestTimeout = flags.Duration("request-timeout", 30*time.Second, "timeout for one upstream Kubernetes API request; 0 disables it")
		retries        = flags.Int("retries", 0, "number of retries for failed upstream requests")
		retryBackoff   = flags.Duration("retry-backoff", 200*time.Millisecond, "delay between upstream request retries")
		helmRelease    = flags.Bool("helm-release-proxy", false, "proxy Helm release storage list/watch requests only through the primary context")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}

	selectedContexts := splitCSV(*contextsCSV)
	targets, primary, err := proxy.LoadTargets(*kubeconfigPath, selectedContexts, *primaryContext)
	if err != nil {
		return err
	}

	bearerToken, err := generateBearerToken()
	if err != nil {
		return err
	}

	handler, err := proxy.NewWithOptions(targets, primary, proxy.Options{
		RequestTimeout:   *requestTimeout,
		Retries:          *retries,
		RetryBackoff:     *retryBackoff,
		BearerToken:      bearerToken,
		HelmReleaseProxy: *helmRelease,
	})
	if err != nil {
		return err
	}

	server := &http.Server{Addr: *listenAddr, Handler: handler}
	listener, err := proxy.Listen(*listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()

	tlsCertificate, certificateAuthorityData, err := generateTLSCertificate(listener.Addr().String())
	if err != nil {
		return err
	}

	serverURL := "https://" + listener.Addr().String()
	if err := kubeconfig.WriteProxyConfig(*outputPath, serverURL, "kubeconfig-proxy", primary.Namespace, bearerToken, certificateAuthorityData); err != nil {
		return err
	}

	log.Printf("source kubeconfig: %s", displayKubeconfigPath(*kubeconfigPath))
	log.Printf("proxy kubeconfig:  %s", *outputPath)
	log.Printf("listen:            %s", serverURL)
	log.Printf("targets:           %s", proxy.TargetNames(targets))
	log.Printf("primary target:    %s", primary.Name)
	log.Printf("request timeout:   %s", durationLogValue(*requestTimeout))
	log.Printf("retries:           %d", *retries)
	log.Printf("retry backoff:     %s", *retryBackoff)
	log.Printf("helm release mode: %t", *helmRelease)

	errCh := make(chan error, 1)
	go func() {
		tlsListener := tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{tlsCertificate},
			MinVersion:   tls.VersionTLS12,
		})
		errCh <- server.Serve(tlsListener)
	}()

	if stop == nil {
		signalStop := make(chan os.Signal, 1)
		signal.Notify(signalStop, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signalStop)
		stop = signalStop
	}
	select {
	case <-stop:
		log.Printf("shutting down")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxy.ShutdownTimeout)
	defer cancel()
	return server.Shutdown(ctx)
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func displayKubeconfigPath(path string) string {
	if path != "" {
		return path
	}
	if value := os.Getenv("KUBECONFIG"); value != "" {
		return value
	}
	return filepath.Join(mustHomeDir(), ".kube", "config")
}

func mustHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Sprintf("detect home dir: %v", err))
	}
	return home
}

func durationLogValue(value time.Duration) string {
	if value <= 0 {
		return "disabled"
	}
	return value.String()
}

func generateBearerToken() (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("generate proxy bearer token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func generateTLSCertificate(addr string) (tls.Certificate, []byte, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse listen address for TLS certificate: %w", err)
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "kubeconfig-proxy",
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	} else if host != "" {
		template.DNSNames = append(template.DNSNames, host)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
		template.DNSNames = append(template.DNSNames, "localhost")
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create TLS certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal TLS private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return certificate, certPEM, nil
}
