GO ?= go
SHELLCHECK ?= shellcheck
GO_TOOLCHAIN ?= go$(shell awk '$$1 == "go" { print $$2; exit }' go.mod)

MAIN_PACKAGE ?= ./cmd/kubeconfig-proxy
PKGS ?= ./...
RACE_PKGS ?= ./internal/proxy
COVER_PROFILE ?= coverage.out
COVER_HTML ?= coverage.html
COVER_PACKAGES ?= ./...

BUILD_DIR ?= bin
BINARY_NAME ?= kubeconfig-proxy

STATICCHECK_VERSION ?= v0.7.0
GOSEC_VERSION ?= v2.28.0
GOVULNCHECK_VERSION ?= v1.6.0
ACTIONLINT_VERSION ?= v1.7.12
YAMLFMT_VERSION ?= v0.21.0
YAMLFMT_FLAGS ?= -formatter retain_line_breaks=true

GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
YAML_FILES := $(shell find . \( -path './.git' -o -path './vendor' -o -path './examples/werf/.helm/templates' \) -prune -o \( -name '*.yaml' -o -name '*.yml' \) -print | sort)
SHELL_FILES := install.sh .codex/skills/test-kubeconfig-proxy/scripts/run.sh
GOTOOLCHAIN_ENV := GOTOOLCHAIN=$(GO_TOOLCHAIN)

.PHONY: help fmt fmt-check yamlfmt yamlfmt-check vet staticcheck actionlint shellcheck gosec vuln test race build build-cover check clean

help:
	@echo "Available targets:"
	@echo "  fmt          Format Go and YAML files"
	@echo "  fmt-check    Verify Go and YAML formatting"
	@echo "  vet          Run go vet"
	@echo "  staticcheck  Run Staticcheck"
	@echo "  actionlint   Run GitHub Actions workflow linting"
	@echo "  shellcheck   Run ShellCheck"
	@echo "  gosec        Run gosec"
	@echo "  vuln         Run govulncheck"
	@echo "  test         Run tests"
	@echo "  race         Run race tests"
	@echo "  build        Build the CLI binary"
	@echo "  build-cover  Build the CLI binary with coverage instrumentation"
	@echo "  check        Run all CI checks"
	@echo "  clean        Remove local build artifacts"

fmt:
	gofmt -w $(GO_FILES)
	$(GOTOOLCHAIN_ENV) $(GO) run github.com/google/yamlfmt/cmd/yamlfmt@$(YAMLFMT_VERSION) $(YAMLFMT_FLAGS) $(YAML_FILES)

fmt-check:
	test -z "$$(gofmt -l $(GO_FILES))"
	$(GOTOOLCHAIN_ENV) $(GO) run github.com/google/yamlfmt/cmd/yamlfmt@$(YAMLFMT_VERSION) $(YAMLFMT_FLAGS) -lint $(YAML_FILES)

vet:
	$(GOTOOLCHAIN_ENV) $(GO) vet $(PKGS)

staticcheck:
	$(GOTOOLCHAIN_ENV) $(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) $(PKGS)

actionlint:
	$(GOTOOLCHAIN_ENV) $(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -verbose

shellcheck:
	$(SHELLCHECK) $(SHELL_FILES)

gosec:
	$(GOTOOLCHAIN_ENV) $(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) $(PKGS)

vuln:
	$(GOTOOLCHAIN_ENV) $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(PKGS)

test:
	$(GOTOOLCHAIN_ENV) $(GO) test -cover -coverprofile=$(COVER_PROFILE) $(PKGS)
	$(GOTOOLCHAIN_ENV) $(GO) tool cover -html=$(COVER_PROFILE) -o $(COVER_HTML)

race:
	$(GOTOOLCHAIN_ENV) $(GO) test -race $(RACE_PKGS)

build:
	mkdir -p $(BUILD_DIR)
	$(GOTOOLCHAIN_ENV) $(GO) build -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PACKAGE)

build-cover:
	mkdir -p $(BUILD_DIR)
	$(GOTOOLCHAIN_ENV) $(GO) build -cover -covermode=atomic -coverpkg=$(COVER_PACKAGES) -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PACKAGE)

check: fmt-check vet staticcheck actionlint shellcheck gosec vuln test race build

clean:
	rm -rf $(BUILD_DIR)
