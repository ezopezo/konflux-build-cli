## Location to install binary dependencies into
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

####################
# Build binary
####################

.PHONY: build
build:
	go build -o konflux-build-cli main.go

# Build statically
.PHONY: build-static
build-static:
	CGO_ENABLED=0 go build -o konflux-build-cli main.go

# Build in debug mode
.PHONY: build-debug
build-debug:
	go build -gcflags "all=-N -l" -o konflux-build-cli main.go

####################
# Tests
####################

INTEGRATION_TEST_TIMEOUT = 20m

# Run all unit tests
.PHONY: unit-test
unit-test:
	go test ./pkg/...

# Run unit tests for a specific package
# Usage: make unit-test-<package-name>
# Examples: make unit-test-commands ; make unit-test-cliwrappers
.PHONY: unit-test-%
unit-test-%:
	go test ./pkg/$*

# Run all integration tests
.PHONY: integration-test
integration-test:
	go test -timeout $(INTEGRATION_TEST_TIMEOUT) ./integration_tests

# Run specific integration test
# Usage: make integration-test-<test-name>
# Examples: make integration-test-TestApplyTags ; make integration-test-TestBuild
.PHONY: integration-test-%
integration-test-%:
	go test -timeout $(INTEGRATION_TEST_TIMEOUT) ./integration_tests -run $*

####################
# Linters
####################

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

# Download golangci-lint locally if necessary
.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) into ./bin/"
	GOTOOLCHAIN=auto GOSUMDB=sum.golang.org GOBIN=$(PWD)/bin/ go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Run golangci-lint
.PHONY: lint
lint: golangci-lint
	$(GOLANGCI_LINT) run

# Run only "fast" subset of linters
.PHONY: lint-fast
lint-fast: golangci-lint
	$(GOLANGCI_LINT) run --fast-only

# Run golangci-lint on a specific file
# Note, it might fail on typecheck due to looking only into one file,
# use lint-package instead in such case.
# Usage: make lint-file FILE=<path-to-file>
# Example: make lint-file FILE=pkg/commands/apply_tags.go
.PHONY: lint-file
lint-file: golangci-lint
	$(GOLANGCI_LINT) run $(FILE)

# Run only "fast" subset of linters on the given file
.PHONY: lint-file-fast
lint-file-fast: golangci-lint
	$(GOLANGCI_LINT) run --fast-only $(FILE)

# Run golangci-lint on a specific package
# Usage: make lint-package PACKAGE=<package-path>
# Example: make lint-package PACKAGE=pkg/common/containerfile_editor
.PHONY: lint-package
lint-package: golangci-lint
	$(GOLANGCI_LINT) run $(PACKAGE)

# Run only "fast" subset of linters on the given package
.PHONY: lint-package-fast
lint-package-fast: golangci-lint
	$(GOLANGCI_LINT) run --fast-only $(PACKAGE)

# Run golangci-lint linter and perform fixes
.PHONY: lint-fix
lint-fix: golangci-lint
	$(GOLANGCI_LINT) run --fix
