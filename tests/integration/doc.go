// Package integration holds CairnOS's live-Postgres tests. They build and run
// only under the `integration` build tag and need a running container runtime
// (Docker or podman): `make test-integration` (or `make test`) starts a Postgres
// container via testcontainers-go through scripts/integration.sh. That script
// resolves the runtime for podman (sets DOCKER_HOST from the podman machine
// socket and disables the Ryuk reaper, which cannot bind-mount that socket) and
// is a no-op on Docker and CI. Plain `go test ./...` (no tag) skips all of this
// and needs no container.
//
// This file carries the package clause for the default (untagged) build so
// `go build ./...` has something to compile when the tag is off; the real
// support code lives in support.go behind the tag.
package integration
