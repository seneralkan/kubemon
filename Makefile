BIN := bin/kubemon
export GOTOOLCHAIN ?= go1.27.4
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/seneralkan/kubemon/cmd.version=$(VERSION) \
	-X github.com/seneralkan/kubemon/cmd.commit=$(shell git rev-parse --short HEAD 2>/dev/null) \
	-X github.com/seneralkan/kubemon/cmd.date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: help build install test lint fmt tidy clean release-snapshot

help: ## Show the available targets (default)
	@grep -hE '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-17s %s\n", $$1, $$2}'

build: ## Build ./bin/kubemon (git-ignored) with version info
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

install: ## Install kubemon into $(go env GOPATH)/bin
	go install -ldflags "$(LDFLAGS)" .

test: ## Unit tests with the race detector
	go test -race ./...

lint: ## golangci-lint (must be built with Go >= 1.27)
	golangci-lint run

fmt: ## gofmt all files
	gofmt -w .

tidy: ## go mod tidy
	go mod tidy

clean: ## Remove bin/, dist/ and completions/
	rm -rf bin dist completions

# e2e runs against the current KUBECONFIG context, which must be a throwaway
# kind cluster (the tests refuse other contexts unless KUBEMON_E2E_CONTEXT names it).
.PHONY: e2e
e2e: build ## End-to-end tests against a throwaway kind cluster
	@echo "WARNING: e2e creates and deletes namespaces in the current KUBECONFIG context; it must be a throwaway cluster."
	KUBEMON_BIN=$(abspath $(BIN)) go test -tags e2e -v -count=1 -timeout 20m ./e2e/...

# Local dry run of the release (needs goreleaser): builds every archive into dist/, publishes nothing.
release-snapshot: ## Dry-run the release into dist/ (needs goreleaser)
	goreleaser release --snapshot --clean --skip=publish
