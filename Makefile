# CairnOS developer tasks.

.DEFAULT_GOAL := help

BINARY := bin/cairnos

# Linters install into bin/tools rather than being declared as go.mod tools:
# golangci-lint pulls a large module graph, and MVS would drag the product
# binary's own dependencies along with it. Neither linter is pinned: `make lint`
# installs the newest golangci-lint and gosec, and .github/workflows/ci.yml asks
# its golangci-lint action for `latest`, so every run uses whatever upstream
# last published.
TOOLS_DIR := bin/tools

# install_latest installs the newest release of package $(1) into bin/tools,
# where go install names the binary after the path's last element. Phony
# targets call it because a file rule would keep the first release installed.
# A failed install, such as a lost proxy connection, falls back to the binary
# already there, so the lint gate fails only without one.
install_latest = GOBIN=$(CURDIR)/$(TOOLS_DIR) go install $(1)@latest || { \
	test -x $(TOOLS_DIR)/$(notdir $(1)) || exit 1; \
	echo "W: could not install the newest $(notdir $(1)), linting with the one in $(TOOLS_DIR)" >&2; \
}

.PHONY: help build deploy test test-unit test-integration test-scripts lint golangci-lint gosec tidy proto update-deps

help: ## List available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "%-17s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: proto ## Build the cairnos binary into bin/
	go build -o $(BINARY) ./cmd/cairnos

deploy: build ## Build, then let `cairnos update --no-fetch` pull secrets and restart on change
	./scripts/deploy.sh

test: proto test-unit test-integration test-scripts ## Run unit then integration tests with the race detector, then the shell script tests

test-unit: proto ## Run unit tests only with the race detector (no container)
	go test -race -count=1 ./...

test-integration: proto ## Run integration tests only (starts a Postgres container via testcontainers)
	./scripts/integration.sh ./tests/integration/...

test-scripts: ## Run the shell script tests against stub tools and fixture modules (needs jq and go, no network)
	./scripts/update-deps_test.sh
	./scripts/model-update-issues_test.sh
	./scripts/third-party-notices_test.sh

lint: gosec golangci-lint ## Run gosec and golangci-lint against the whole tree
	$(TOOLS_DIR)/golangci-lint version
	$(TOOLS_DIR)/golangci-lint run

# gosec is its own target because CI runs it on its own: the workflow gets
# golangci-lint from an action, and this keeps the exclude list in one place.
# Not -quiet: with it, gosec exits 0 whenever it finds no issues, before it
# checks for packages that failed to load. Without it, a failed load fails the
# target.
gosec: ## Run gosec alone (its golangci integration deadlocks on multi-package runs)
	$(call install_latest,github.com/securego/gosec/v2/cmd/gosec)
	$(TOOLS_DIR)/gosec -exclude-dir=internal/api/gen ./...

golangci-lint: ## Install the newest golangci-lint into bin/tools
	$(call install_latest,github.com/golangci/golangci-lint/v2/cmd/golangci-lint)

proto: ## Regenerate gRPC, gateway, and OpenAPI code from proto/ (buf and its plugins are go.mod tools, nothing to install)
	go tool buf generate

tidy: ## Sync go.mod and go.sum with the source
	go mod tidy

update-deps: ## Upgrade every dependency and pin to its newest release, except the module holds scripts/update-deps.sh explains (modules, tools, buf, web, actions, postgres, Bitwarden CLI)
	./scripts/update-deps.sh
