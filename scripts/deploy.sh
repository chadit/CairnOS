#!/usr/bin/env bash
set -euo pipefail
trap 'echo "E: deploy failed on line $LINENO" >&2' ERR

# deploy.sh - a wrapper around `cairnos update`, which carries the logic:
# rebuild, pull secrets, and restart the service when either moved. --no-fetch
# is passed because someone running this from a clone is deploying the checkout
# in front of them rather than whatever origin holds.
#
# CAIRNOS_HOME still selects the home, so a box running two instances deploys
# each one without editing this script.
#
# Usage:
#   scripts/deploy.sh          # build, pull, restart on change
#   scripts/deploy.sh --check  # report only; exit 3 when behind

readonly BINARY="bin/cairnos"

# An array rather than an unquoted expansion: the flag and its value have to
# reach the command as two words without the shell splitting the path itself.
HOME_ARGS=()
if [[ -n "${CAIRNOS_HOME:-}" ]]; then
	HOME_ARGS=(--dir "$CAIRNOS_HOME")
fi
readonly HOME_ARGS

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

# The wrapper has to bootstrap once: `cairnos update` runs through the binary,
# so a clone that has never been built has nothing to run.
if [[ ! -x "$BINARY" ]]; then
	echo "I: bootstrapping $BINARY" >&2
	make --no-print-directory build >&2
fi

exec "$BINARY" update --no-fetch "${HOME_ARGS[@]}" "$@"
