#!/usr/bin/env bash
set -euo pipefail
trap 'echo "E: deploy failed on line $LINENO" >&2' ERR

# deploy.sh - pull secrets, rebuild when the running binary is behind HEAD, and
# restart the cairnos user service when either changed. Uses the vcs.revision
# stamp Go embeds in every build, so nothing else has to carry a version.
#
# Usage:
#   scripts/deploy.sh          # pull, build when behind, restart on change
#   scripts/deploy.sh --check  # report only, no pull; exit 3 when behind

readonly UNIT="cairnos"
readonly BINARY="bin/cairnos"
readonly START_TIMEOUT_SECONDS=30

# CAIRNOS_HOME is where the unit reads its config and both environment files
# from. It is overridable so a second instance on one box can be deployed
# without editing this script.
CAIRNOS_HOME="${CAIRNOS_HOME:-$HOME/.cairnos}"
readonly CAIRNOS_HOME

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

# secrets_digest prints the sha256 of the secrets file, nothing when absent.
# Returns 0 either way.
secrets_digest() {
	local path="$CAIRNOS_HOME/secrets.env"
	if [[ ! -f "$path" ]]; then
		return 0
	fi
	sha256sum -- "$path" | awk '{ print $1 }'
}

# pull_secrets renders the vault fields into $CAIRNOS_HOME, leaving the file
# alone on a host with no [secrets.bitwarden] block. Returns 0 or dies.
pull_secrets() {
	info "pulling secrets into $CAIRNOS_HOME"
	"$BINARY" secrets pull --dir "$CAIRNOS_HOME" >&2 ||
		die "secrets pull failed; $UNIT was left running the previous build"
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

# restart_unit restarts the service and checks the revision it comes back on.
# Returns 0 or dies.
restart_unit() {
	local wanted="$1" old_pid="$2" now
	info "restarting $UNIT"
	systemctl --user restart -- "$UNIT"
	wait_for_serve "$old_pid"
	now="$(running_revision)"
	[[ "$now" == "$wanted" ]] || die "service is running ${now:-nothing}, wanted ${wanted}"
	info "$UNIT is running ${now}"
}

# deploy builds when behind and pulls secrets every run, so a rotated field
# reaches the service without a code change. The pull runs the binary, so a
# missing one needs a build even at HEAD, and the pull comes before the restart
# so the service starts on the new values. Returns 0 or dies.
deploy() {
	local wanted="$1" behind="$2" old_pid before after
	old_pid="$(unit_pid)"
	if ((behind)) || [[ ! -x "$BINARY" ]]; then
		info "building $BINARY at ${wanted}"
		make --no-print-directory build >&2
	fi
	before="$(secrets_digest)"
	pull_secrets
	after="$(secrets_digest)"
	if ((behind == 0)) && [[ "$before" == "$after" ]]; then
		info "secrets unchanged; $UNIT keeps running ${wanted}"
		return
	fi
	restart_unit "$wanted" "$old_pid"
}

# main compares the running revision against HEAD and deploys the difference.
# --check never pulls, so it stays safe on a host you only inspect: it reports
# and exits 3 when behind. Returns 0 or dies.
main() {
	local check_only=0 wanted running behind=0
	if [[ "${1:-}" == "--check" ]]; then
		check_only=1
	fi
	cd -- "$(git rev-parse --show-toplevel)"

	wanted="$(wanted_revision)"
	running="$(running_revision)"
	if [[ "$running" != "$wanted" || "$wanted" == *+dirty ]]; then
		behind=1
	fi

	if ((check_only)); then
		if ((behind == 0)); then
			info "$UNIT already runs ${running}"
			return
		fi
		info "running: ${running:-not running}"
		info "wanted:  ${wanted}"
		exit 3
	fi

	command -v sha256sum >/dev/null 2>&1 || die "sha256sum required"
	if ((behind)); then
		info "running: ${running:-not running}"
		info "wanted:  ${wanted}"
	else
		info "$UNIT already runs ${running}"
	fi
	deploy "$wanted" "$behind"
}

main "$@"
