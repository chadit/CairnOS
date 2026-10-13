# CairnOS developer tasks.

.DEFAULT_GOAL := help

BINARY := bin/cairnos

# Linters install into bin/tools rather than being declared as go.mod tools:
# golangci-lint pulls a large module graph, and MVS would drag the product
# binary's own dependencies along with it. golangci-lint is not pinned: every
# `make lint` installs the newest release, and .github/workflows/ci.yml asks
# its action for `latest`, so both run whatever upstream last published. gosec
# keeps a pin, which scripts/update-deps.sh rewrites.
GOSEC_VERSION := v2.29.0

TOOLS_DIR := bin/tools

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

test-scripts: ## Run the shell script tests against stub tools (needs jq, no network)
	./scripts/update-deps_test.sh
	./scripts/model-update-issues_test.sh

lint: gosec golangci-lint ## Run gosec and golangci-lint against the whole tree
	$(TOOLS_DIR)/golangci-lint version
	$(TOOLS_DIR)/golangci-lint run

# gosec is its own target because CI runs it on its own: the workflow gets
# golangci-lint from an action, and this keeps the exclude list in one place
# rather than repeated in the workflow.
gosec: $(TOOLS_DIR)/gosec ## Run gosec alone (its golangci integration deadlocks on multi-package runs)
	$(TOOLS_DIR)/gosec -quiet -exclude-dir=internal/api/gen ./...

# Phony on purpose: a file rule would keep whatever release was installed first,
# and the point of @latest is that the binary follows upstream. Each run asks the
# module proxy for the newest release. When bin/tools already holds it, the module
# cache skips the download and the build cache skips the compile, so a run costs
# the proxy lookup. A failed install, such as a lost proxy connection, falls back
# to the binary bin/tools already has, so the lint gate fails only without one.
golangci-lint: ## Install the newest golangci-lint into bin/tools
	GOBIN=$(CURDIR)/$(TOOLS_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest || { \
		test -x $(TOOLS_DIR)/golangci-lint || exit 1; \
		echo "W: could not install the newest golangci-lint, linting with the one in $(TOOLS_DIR)" >&2; \
	}

# Keyed on this Makefile, the only place the version is written, so a pin change
# reinstalls rather than leaving the previous binary in bin/tools. Any other edit
# here reinstalls too, which costs a minute and is the price of not tracking the
# variable separately.
$(TOOLS_DIR)/gosec: Makefile
	GOBIN=$(CURDIR)/$(TOOLS_DIR) go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)

proto: ## Regenerate gRPC, gateway, and OpenAPI code from proto/ (buf and its plugins are go.mod tools, nothing to install)
	go tool buf generate

tidy: ## Sync go.mod and go.sum with the source
	go mod tidy

update-deps: ## Upgrade every dependency and pin to its newest release, except the module holds scripts/update-deps.sh explains (modules, tools, buf, web, gosec, actions, postgres, Bitwarden CLI)
	./scripts/update-deps.sh
