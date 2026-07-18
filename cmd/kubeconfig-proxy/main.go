package main

import (
	"fmt"
	"io"
	"log"
	"os"
)

var cliVersion = "unknown"

func main() {
	if err := run(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run() error {
	return runWithArgs(os.Args[1:], nil)
}

func runWithArgs(args []string, stop <-chan os.Signal) error {
	if len(args) > 0 {
		switch args[0] {
		case "add-context":
			return runAddContext(args[1:])
		case "delete-context":
			return runDeleteContext(args[1:])
		case "credential":
			return runCredential(args[1:])
		case "serve":
			return runServeState(args[1:], stop)
		case "version":
			return runVersion(args[1:], os.Stdout)
		}
	}
	return fmt.Errorf("usage: kubeconfig-proxy <add-context|delete-context|credential|serve|version> [flags]")
}

func runVersion(args []string, out io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: kubeconfig-proxy version")
	}
	_, err := fmt.Fprintln(out, cliVersion)
	return err
}
