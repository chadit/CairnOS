#!/usr/bin/env bash
# Run the CairnOS integration tests against a container runtime.
#
# CI and Docker Desktop expose the default socket, so testcontainers finds the
# runtime with no help. Rootless podman (common on macOS) is different: the
# machine socket is not auto-discovered, and the testcontainers Ryuk reaper
# cannot bind-mount that socket. So when nothing else has set DOCKER_HOST and the
# default Docker socket is absent, point DOCKER_HOST at the podman socket and
# disable Ryuk. The suite terminates its own container in TestMain, so a disabled
# reaper still leaves nothing behind.
set -euo pipefail

trap 'echo "integration.sh: error on line $LINENO" >&2' ERR

# resolve_runtime sets DOCKER_HOST for podman when nothing else has provided a
# reachable Docker API. It is a no-op on Docker and CI.
resolve_runtime() {
	if [[ -n "${DOCKER_HOST:-}" ]]; then
		return
	fi

	if [[ -S /var/run/docker.sock ]]; then
		return
	fi

	if ! command -v podman >/dev/null 2>&1; then
		return
	fi

	local sock
	sock="$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}' 2>/dev/null || true)"

	if [[ -n "$sock" && -S "$sock" ]]; then
		export DOCKER_HOST="unix://$sock"
		export TESTCONTAINERS_RYUK_DISABLED=true
	fi
}

resolve_runtime

exec go test -tags integration -race "$@"
