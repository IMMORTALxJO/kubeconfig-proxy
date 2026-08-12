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
SHELL_FILES := install.sh e2e/run.sh e2e/run-upstream-kubectl-e2e.sh e2e/versions.sh e2e/versions_test.sh $(wildcard e2e/checks/*.sh)
GOTOOLCHAIN_ENV := GOTOOLCHAIN=$(GO_TOOLCHAIN)
E2E_VARIANT_GOALS := $(filter local kubectl kind,$(MAKECMDGOALS))

.PHONY: help fmt fmt-check yamlfmt yamlfmt-check vet staticcheck actionlint shellcheck gosec vuln test race build build-cover check e2e-selection-test e2e-prefix-test e2e-versions-test clean e2e local kubectl kind

ifneq ($(strip $(E2E_VARIANT_GOALS)),)
ifneq ($(words $(E2E_VARIANT_GOALS)),1)
$(error use one e2e variant: make e2e local, make e2e kind, or make e2e kubectl)
endif
endif

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
	@echo "  e2e-selection-test  Test targeted e2e check selection"
	@echo "  e2e-prefix-test  Test e2e resource-prefix validation"
	@echo "  e2e-versions-test  Test compatibility version profiles"
	@echo "  check        Run all CI checks"

	@echo "  e2e          Run all e2e suites (kind, then upstream kubectl)"
	@echo "  e2e local    Run built-in kind checks without make check or werf"
	@echo "  e2e kind     Run the two-cluster kind integration suite"
	@echo "  e2e kubectl  Run the upstream kubectl compatibility suite"
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

check: fmt-check vet staticcheck actionlint shellcheck gosec vuln test race build e2e-selection-test e2e-prefix-test e2e-versions-test

e2e-selection-test:
	bash e2e/checks/selection_test.sh

e2e-prefix-test:
	bash e2e/checks/prefix_test.sh

e2e-versions-test:
	bash e2e/versions_test.sh

ifeq ($(strip $(E2E_VARIANT_GOALS)),)
e2e:
	$(MAKE) --no-print-directory kind
	$(MAKE) --no-print-directory kubectl
else
e2e:
	@:
endif

local:
	KCP_SKIP_MAKE_CHECK=1 KCP_SKIP_WERF=1 e2e/run.sh

kind:
	e2e/run.sh

kubectl:
	e2e/run-upstream-kubectl-e2e.sh

clean:
	rm -rf $(BUILD_DIR)
