GO ?= go
GO_TOOLCHAIN ?= go$(shell awk '$$1 == "go" { print $$2; exit }' go.mod)

MAIN_PACKAGE ?= ./cmd/kubeconfig-proxy
PKGS ?= ./...
RACE_PKGS ?= ./internal/proxy

BUILD_DIR ?= bin
BINARY_NAME ?= kubeconfig-proxy

STATICCHECK_VERSION ?= v0.7.0
GOSEC_VERSION ?= v2.28.0
GOVULNCHECK_VERSION ?= v1.5.0

GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
GOTOOLCHAIN_ENV := GOTOOLCHAIN=$(GO_TOOLCHAIN)

.PHONY: help fmt fmt-check vet staticcheck gosec vuln test race build check clean

help:
	@echo "Available targets:"
	@echo "  fmt          Format Go files"
	@echo "  fmt-check    Verify Go formatting"
	@echo "  vet          Run go vet"
	@echo "  staticcheck  Run Staticcheck"
	@echo "  gosec        Run gosec"
	@echo "  vuln         Run govulncheck"
	@echo "  test         Run tests"
	@echo "  race         Run race tests"
	@echo "  build        Build the CLI binary"
	@echo "  check        Run all CI checks"
	@echo "  clean        Remove local build artifacts"

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	test -z "$$(gofmt -l $(GO_FILES))"

vet:
	$(GOTOOLCHAIN_ENV) $(GO) vet $(PKGS)

staticcheck:
	$(GOTOOLCHAIN_ENV) $(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) $(PKGS)

gosec:
	$(GOTOOLCHAIN_ENV) $(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) $(PKGS)

vuln:
	$(GOTOOLCHAIN_ENV) $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(PKGS)

test:
	$(GOTOOLCHAIN_ENV) $(GO) test $(PKGS)

race:
	$(GOTOOLCHAIN_ENV) $(GO) test -race $(RACE_PKGS)

build:
	mkdir -p $(BUILD_DIR)
	$(GOTOOLCHAIN_ENV) $(GO) build -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PACKAGE)

check: fmt-check vet staticcheck gosec vuln test race build

clean:
	rm -rf $(BUILD_DIR)
