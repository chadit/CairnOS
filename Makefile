# CairnOS developer tasks.

.DEFAULT_GOAL := help

BINARY := bin/cairnos

.PHONY: help build test integration lint tidy proto

help: ## List available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "%-8s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the cairnos binary into bin/
	go build -o $(BINARY) ./cmd/cairnos

test: ## Run unit and integration tests with the race detector (integration starts a Postgres container)
	./scripts/integration.sh ./...

integration: ## Run only the integration tests (starts a Postgres container via testcontainers)
	./scripts/integration.sh ./tests/integration/...

lint: ## Run golangci-lint, then gosec standalone (its golangci integration deadlocks on multi-package runs)
	golangci-lint run
	gosec -quiet -exclude-dir=internal/api/gen ./...

proto: ## Regenerate gRPC, gateway, and OpenAPI code from proto/ (plugins pinned via go.mod tool directives)
	buf generate

tidy: ## Sync go.mod and go.sum with the source
	go mod tidy
