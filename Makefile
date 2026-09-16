# CairnOS developer tasks.

.DEFAULT_GOAL := help

BINARY := bin/cairnos

# Linter pins. Both tools are installed into bin/tools rather than declared as
# go.mod tools: golangci-lint pulls a large module graph, and MVS would drag the
# product binary's own dependencies along with it. scripts/update-deps.sh
# rewrites these two lines, and .github/workflows/ci.yml reads the first one, so
# CI and a developer machine run the same golangci-lint.
GOLANGCI_LINT_VERSION := v2.13.2
GOSEC_VERSION         := v2.29.0

TOOLS_DIR := bin/tools

.PHONY: help build deploy test test-unit test-integration lint gosec tidy proto update-dep

help: ## List available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "%-17s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: proto ## Build the cairnos binary into bin/
	go build -o $(BINARY) ./cmd/cairnos

deploy: ## Rebuild and restart the cairnos user service when it is not running HEAD
	./scripts/deploy.sh

test: proto test-unit test-integration ## Run unit then integration tests with the race detector

test-unit: proto ## Run unit tests only with the race detector (no container)
	go test -race -count=1 ./...

test-integration: proto ## Run integration tests only (starts a Postgres container via testcontainers)
	./scripts/integration.sh ./tests/integration/...

lint: gosec $(TOOLS_DIR)/golangci-lint ## Run gosec and golangci-lint against the whole tree
	$(TOOLS_DIR)/golangci-lint run

# gosec is its own target because CI runs it on its own: the workflow gets
# golangci-lint from an action, and this keeps the exclude list in one place
# rather than repeated in the workflow.
gosec: $(TOOLS_DIR)/gosec ## Run gosec alone (its golangci integration deadlocks on multi-package runs)
	$(TOOLS_DIR)/gosec -quiet -exclude-dir=internal/api/gen ./...

# Both rules key on this Makefile, which is the only place the versions are
# written, so a version change reinstalls rather than leaving the previous
# binary in bin/tools. Any other edit here reinstalls too, which costs a minute
# and is the price of not tracking the variables separately.
$(TOOLS_DIR)/golangci-lint: Makefile
	GOBIN=$(CURDIR)/$(TOOLS_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(TOOLS_DIR)/gosec: Makefile
	GOBIN=$(CURDIR)/$(TOOLS_DIR) go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)

proto: ## Regenerate gRPC, gateway, and OpenAPI code from proto/ (buf and its plugins are go.mod tools, nothing to install)
	go tool buf generate

tidy: ## Sync go.mod and go.sum with the source
	go mod tidy

update-dep: ## Upgrade Go modules, tools, buf deps, web packages, the linter pins, and the Bitwarden CLI pin
	./scripts/update-deps.sh
