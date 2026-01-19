# Makefile for maestro-cli

# Project metadata
PROJECT_NAME := maestro-cli
VERSION ?= 0.1.0
IMAGE_REGISTRY ?= ghcr.io/hyperfleet
IMAGE_TAG ?= latest

# Build metadata
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_TAG := $(shell git describe --tags --exact-match 2>/dev/null || echo "")
BUILD_DATE := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

# Dev image configuration - set QUAY_USER to push to personal registry
# Usage: QUAY_USER=myuser make image-dev
QUAY_USER ?=
DEV_TAG ?= dev-$(GIT_COMMIT)

# LDFLAGS for build
# Note: Variables are in package main, so use main.varName (not full import path)
LDFLAGS := -w -s
LDFLAGS += -X main.version=$(VERSION)
LDFLAGS += -X main.commit=$(GIT_COMMIT)
LDFLAGS += -X main.buildDate=$(BUILD_DATE)
ifneq ($(GIT_TAG),)
LDFLAGS += -X main.tag=$(GIT_TAG)
endif

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOMOD := $(GOCMD) mod
GOFMT := gofmt

# Test parameters
TEST_TIMEOUT := 30m
RACE_FLAG := -race
COVERAGE_OUT := coverage.out
COVERAGE_HTML := coverage.html

# Container runtime detection
DOCKER_AVAILABLE := $(shell if docker info >/dev/null 2>&1; then echo "true"; else echo "false"; fi)
PODMAN_AVAILABLE := $(shell if podman info >/dev/null 2>&1; then echo "true"; else echo "false"; fi)

ifeq ($(DOCKER_AVAILABLE),true)
    CONTAINER_RUNTIME := docker
    CONTAINER_CMD := docker
else ifeq ($(PODMAN_AVAILABLE),true)
    CONTAINER_RUNTIME := podman
    CONTAINER_CMD := podman
    # Find Podman socket for testcontainers compatibility
    PODMAN_SOCK := $(shell find /var/folders -name "podman-machine-*-api.sock" 2>/dev/null | head -1)
    ifeq ($(PODMAN_SOCK),)
        PODMAN_SOCK := $(shell find ~/.local/share/containers/podman/machine -name "*.sock" 2>/dev/null | head -1)
    endif
    ifneq ($(PODMAN_SOCK),)
        export DOCKER_HOST := unix://$(PODMAN_SOCK)
        export TESTCONTAINERS_RYUK_DISABLED := true
    endif
else
    CONTAINER_RUNTIME := none
    CONTAINER_CMD := sh -c 'echo "No container runtime found. Please install Docker or Podman." && exit 1'
endif

# Install directory (defaults to $GOPATH/bin or $HOME/go/bin)
GOPATH ?= $(shell $(GOCMD) env GOPATH)
BINDIR ?= $(GOPATH)/bin

# Directories
# Find all Go packages, excluding vendor and test directories
PKG_DIRS := $(shell $(GOCMD) list ./... 2>/dev/null | grep -v /vendor/ | grep -v /test/)

# Include bingo-managed tool versions
include .bingo/Variables.mk

.PHONY: help
help: ## Display this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: test
test: ## Run unit tests with race detection
	@echo "Running unit tests..."
	$(GOTEST) -v $(RACE_FLAG) -timeout $(TEST_TIMEOUT) $(PKG_DIRS)

.PHONY: test-coverage
test-coverage: ## Run unit tests with coverage report
	@echo "Running unit tests with coverage..."
	$(GOTEST) -v $(RACE_FLAG) -timeout $(TEST_TIMEOUT) -coverprofile=$(COVERAGE_OUT) -covermode=atomic $(PKG_DIRS)
	@echo "Coverage report generated: $(COVERAGE_OUT)"
	@echo "To view HTML coverage report, run: make test-coverage-html"

.PHONY: test-coverage-html
test-coverage-html: test-coverage ## Generate HTML coverage report
	@echo "Generating HTML coverage report..."
	$(GOCMD) tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)
	@echo "HTML coverage report generated: $(COVERAGE_HTML)"

.PHONY: test-all
test-all: test lint ## Run all tests and checks (unit + lint)
	@echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	@echo "✅ All tests completed successfully!"
	@echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint
	@echo "Running golangci-lint..."
	$(GOLANGCI_LINT) cache clean && $(GOLANGCI_LINT) run

.PHONY: fmt
fmt: $(GOIMPORTS) ## Format code with gofmt and goimports
	@echo "Formatting code..."
	$(GOIMPORTS) -w .

.PHONY: mod-tidy
mod-tidy: ## Tidy Go module dependencies
	@echo "Tidying Go modules..."
	$(GOMOD) tidy
	$(GOMOD) verify

.PHONY: binary
binary: ## Build binary
	@echo "Building $(PROJECT_NAME)..."
	@echo "Version: $(VERSION), Commit: $(GIT_COMMIT), BuildDate: $(BUILD_DATE)"
	@mkdir -p bin
	CGO_ENABLED=0 $(GOBUILD) -ldflags="$(LDFLAGS)" -o bin/$(PROJECT_NAME) ./cmd/$(PROJECT_NAME)

.PHONY: build
build: binary ## Alias for 'binary'

.PHONY: install
install: binary ## Install binary to BINDIR (default: $GOPATH/bin)
	@echo "Installing $(PROJECT_NAME) to $(BINDIR)..."
	@mkdir -p $(BINDIR)
	cp bin/$(PROJECT_NAME) $(BINDIR)/$(PROJECT_NAME)
	@echo "✅ Installed: $(BINDIR)/$(PROJECT_NAME)"

.PHONY: run
run: binary ## Build and run the application
	./bin/$(PROJECT_NAME)

.PHONY: clean
clean: ## Clean build artifacts and test coverage files
	@echo "Cleaning..."
	rm -rf bin/
	rm -f $(COVERAGE_OUT) $(COVERAGE_HTML)

.PHONY: image
image: ## Build container image with Docker or Podman
ifeq ($(CONTAINER_RUNTIME),none)
	@echo "❌ ERROR: No container runtime found"
	@echo "Please install Docker or Podman"
	@exit 1
else
	@echo "Building container image with $(CONTAINER_RUNTIME)..."
	$(CONTAINER_CMD) build --platform linux/amd64 --no-cache --build-arg GIT_COMMIT=$(GIT_COMMIT) -t $(IMAGE_REGISTRY)/$(PROJECT_NAME):$(IMAGE_TAG) .
	@echo "✅ Image built: $(IMAGE_REGISTRY)/$(PROJECT_NAME):$(IMAGE_TAG)"
endif

.PHONY: image-push
image-push: image ## Build and push container image to registry
ifeq ($(CONTAINER_RUNTIME),none)
	@echo "❌ ERROR: No container runtime found"
	@echo "Please install Docker or Podman"
	@exit 1
else
	@echo "Pushing image $(IMAGE_REGISTRY)/$(PROJECT_NAME):$(IMAGE_TAG)..."
	$(CONTAINER_CMD) push $(IMAGE_REGISTRY)/$(PROJECT_NAME):$(IMAGE_TAG)
	@echo "✅ Image pushed: $(IMAGE_REGISTRY)/$(PROJECT_NAME):$(IMAGE_TAG)"
endif

.PHONY: image-dev
image-dev: ## Build and push to personal Quay registry (requires QUAY_USER)
ifndef QUAY_USER
	@echo "❌ ERROR: QUAY_USER is not set"
	@echo ""
	@echo "Usage: QUAY_USER=myuser make image-dev"
	@echo ""
	@echo "This will build and push to: quay.io/$$QUAY_USER/$(PROJECT_NAME):$(DEV_TAG)"
	@exit 1
endif
ifeq ($(CONTAINER_RUNTIME),none)
	@echo "❌ ERROR: No container runtime found"
	@echo "Please install Docker or Podman"
	@exit 1
else
	@echo "Building dev image quay.io/$(QUAY_USER)/$(PROJECT_NAME):$(DEV_TAG)..."
	$(CONTAINER_CMD) build --platform linux/amd64 --build-arg GIT_COMMIT=$(GIT_COMMIT) -t quay.io/$(QUAY_USER)/$(PROJECT_NAME):$(DEV_TAG) .
	@echo "Pushing dev image..."
	$(CONTAINER_CMD) push quay.io/$(QUAY_USER)/$(PROJECT_NAME):$(DEV_TAG)
	@echo ""
	@echo "✅ Dev image pushed: quay.io/$(QUAY_USER)/$(PROJECT_NAME):$(DEV_TAG)"
endif

# Cross-compilation targets
.PHONY: build-linux
build-linux: ## Build for Linux (amd64)
	@echo "Building for Linux amd64..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -ldflags="$(LDFLAGS)" -o bin/$(PROJECT_NAME)-linux-amd64 ./cmd/$(PROJECT_NAME)

.PHONY: build-darwin
build-darwin: ## Build for macOS (arm64)
	@echo "Building for macOS arm64..."
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GOBUILD) -ldflags="$(LDFLAGS)" -o bin/$(PROJECT_NAME)-darwin-arm64 ./cmd/$(PROJECT_NAME)

.PHONY: build-windows
build-windows: ## Build for Windows (amd64)
	@echo "Building for Windows amd64..."
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GOBUILD) -ldflags="$(LDFLAGS)" -o bin/$(PROJECT_NAME)-windows-amd64.exe ./cmd/$(PROJECT_NAME)

.PHONY: build-all
build-all: build-linux build-darwin build-windows ## Build for all platforms
	@echo "✅ All binaries built in bin/"

.PHONY: verify
verify: lint test ## Run all verification checks (lint + test)
