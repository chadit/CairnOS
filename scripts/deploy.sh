#!/usr/bin/env bash
set -euo pipefail
trap 'echo "E: deploy failed on line $LINENO" >&2' ERR

# deploy.sh - rebuild and restart the cairnos user service when the running
# binary is not built from HEAD. Uses the vcs.revision stamp Go embeds in
# every build, so nothing else has to carry a version.
#
# Usage:
#   scripts/deploy.sh          # build and restart when behind
#   scripts/deploy.sh --check  # report only; exit 3 when behind

readonly UNIT="cairnos"
readonly BINARY="bin/cairnos"
readonly START_TIMEOUT_SECONDS=30

info() { echo "I: $*" >&2; }

die() {
	echo "E: $*" >&2
	exit 1
}

# stamped_revision prints "<sha>" or "<sha>+dirty" for a Go binary, nothing
# when it carries no stamp.
stamped_revision() {
	local binary="$1"
	go version -m -- "$binary" 2>/dev/null | awk '
		$1 == "build" && $2 ~ /^vcs\.revision=/ { sub("vcs.revision=", "", $2); rev = $2 }
		$1 == "build" && $2 == "vcs.modified=true" { dirty = "+dirty" }
		END { if (rev != "") print rev dirty }
	'
}

unit_pid() {
	systemctl --user show -p MainPID --value -- "$UNIT" 2>/dev/null || echo 0
}

# running_revision reads the binary through /proc so a file replaced on disk
# is not mistaken for the one in memory. Prints nothing when not running.
running_revision() {
	local pid
	pid="$(unit_pid)"
	if [[ -z "$pid" || "$pid" == "0" ]]; then
		return
	fi
	stamped_revision "/proc/$pid/exe"
}

# wanted_revision spells HEAD the way the Go stamp does: "+dirty" on any
# change, untracked files included, since cmd/go checks a plain
# `git status --porcelain`.
wanted_revision() {
	local head suffix=""
	head="$(git rev-parse HEAD)"
	if [[ -n "$(git status --porcelain)" ]]; then
		suffix="+dirty"
	fi
	echo "${head}${suffix}"
}

# wait_for_serve returns once the unit is active under a new pid, or dies.
wait_for_serve() {
	local old_pid="$1" waited=0 pid
	while ((waited < START_TIMEOUT_SECONDS)); do
		pid="$(unit_pid)"
		if [[ "$pid" != "0" && "$pid" != "$old_pid" ]] && systemctl --user is-active --quiet -- "$UNIT"; then
			return
		fi
		sleep 1
		waited=$((waited + 1))
	done
	die "$UNIT did not come back within ${START_TIMEOUT_SECONDS}s; see: journalctl --user -u $UNIT -n 50"
}

deploy() {
	local wanted="$1" old_pid now
	old_pid="$(unit_pid)"
	info "building $BINARY at ${wanted}"
	make --no-print-directory build >&2
	info "restarting $UNIT"
	systemctl --user restart -- "$UNIT"
	wait_for_serve "$old_pid"
	now="$(running_revision)"
	[[ "$now" == "$wanted" ]] || die "service is running ${now:-nothing}, wanted ${wanted}"
	info "$UNIT is running ${now}"
}

main() {
	local check_only=0 wanted running
	if [[ "${1:-}" == "--check" ]]; then
		check_only=1
	fi
	cd -- "$(git rev-parse --show-toplevel)"

	wanted="$(wanted_revision)"
	running="$(running_revision)"
	if [[ "$running" == "$wanted" && "$wanted" != *+dirty ]]; then
		info "$UNIT already runs ${running}"
		return
	fi
	info "running: ${running:-not running}"
	info "wanted:  ${wanted}"
	if ((check_only)); then
		exit 3
	fi
	deploy "$wanted"
}

main "$@"
