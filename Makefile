# CairnOS developer tasks.

.DEFAULT_GOAL := help

BINARY := bin/cairnos

.PHONY: help build deploy test test-unit test-integration lint tidy proto

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

lint: ## Run golangci-lint, then gosec standalone (its golangci integration deadlocks on multi-package runs)
	golangci-lint run
	gosec -quiet -exclude-dir=internal/api/gen ./...

proto: ## Regenerate gRPC, gateway, and OpenAPI code from proto/ (buf and its plugins are go.mod tools, nothing to install)
	go tool buf generate

tidy: ## Sync go.mod and go.sum with the source
	go mod tidy
