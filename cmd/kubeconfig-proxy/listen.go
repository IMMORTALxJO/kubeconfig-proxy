package main

import (
	"fmt"
	"net"
	"strings"
)

func resolveAddContextListenAddr(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return pickAvailableListenAddr()
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("parse listen address: %w", err)
	}
	if port == "0" {
		return pickAvailableListenAddrForHost(host)
	}
	return value, nil
}

func pickAvailableListenAddr() (string, error) {
	return pickAvailableListenAddrForHost("127.0.0.1")
}

func pickAvailableListenAddrForHost(host string) (string, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}
