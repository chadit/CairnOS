#!/usr/bin/env bash
set -euo pipefail
trap 'echo "E: third-party-notices_test failed on line $LINENO" >&2' ERR

# third-party-notices_test.sh - exercise third-party-notices.sh against a
# fixture module tree.
#
# Each case copies the real script into a scratch main module whose
# dependencies are local directories pulled in through replace directives, and
# runs it with the real go command. That checks the go list template and the
# per-target package set without touching the network or the module cache.
#
# Usage:
#   scripts/third-party-notices_test.sh
#
# Requires: go. Touches no network and no repository state.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly SUBJECT="${SCRIPT_DIR}/third-party-notices.sh"

WORK_DIR="$(mktemp -d)"
readonly WORK_DIR

FAILURES=0

# ─── Logging ────────────────────────────────────────────────────────────────

# info logs a progress line on stderr. Returns 0.
info() { echo "I: $*" >&2; }

# die logs why the run cannot go on and stops it. Exits 1, never returns.
die() {
	echo "E: $*" >&2
	exit 1
}

# fail records one broken expectation and keeps the run going, so a single
# invocation reports every problem instead of only the first.
# Returns 0 always.
fail() {
	echo "E: $*" >&2
	FAILURES=$((FAILURES + 1))
}

# cleanup removes every scratch module on any exit path. Returns 0.
cleanup() {
	rm -rf -- "$WORK_DIR"
}

trap cleanup EXIT

# ─── Fixtures ───────────────────────────────────────────────────────────────

# write_file creates a file and its parent directories.
# Args: path, contents. Returns 0 on success.
write_file() {
	mkdir -p -- "$(dirname -- "$1")"
	printf '%s\n' "$2" >"$1"
}

# make_dependency writes a one-package module under deps/.
# Args: repository path, module name, package directory inside the module.
# Returns 0 on success.
make_dependency() {
	local repo="$1" name="$2" pkg="$3"
	local dir="${repo}/deps/${name}"
	write_file "${dir}/go.mod" "module example.com/${name}

go 1.22"
	write_file "${dir}/${pkg}/${pkg##*/}.go" "package ${pkg##*/}

func Use() {}"
}

# make_repo builds a main module whose ./cmd/cairnos imports two modules.
# alpha is linked everywhere, through a package below a nested LICENSE, and
# also holds root license files, a license.go, and an unlinked directory whose
# LICENSE must stay out. winonly is linked only when GOOS is windows.
# Args: case name. Returns 0 and prints the repository path.
make_repo() {
	local name="$1"
	local repo="${WORK_DIR}/${name}"

	mkdir -p -- "${repo}/scripts"
	cp -- "$SUBJECT" "${repo}/scripts/third-party-notices.sh"

	make_dependency "$repo" alpha vendored/inner
	write_file "${repo}/deps/alpha/LICENSE" "alpha root license text"
	write_file "${repo}/deps/alpha/NOTICE" "alpha apache notice text"
	write_file "${repo}/deps/alpha/LICENSE-3RD-PARTY.md" "alpha third-party text"
	write_file "${repo}/deps/alpha/license.go" "package alpha"
	write_file "${repo}/deps/alpha/vendored/LICENSE" "alpha vendored license text"
	write_file "${repo}/deps/alpha/unused/LICENSE" "alpha unused license text"

	make_dependency "$repo" winonly winonly
	write_file "${repo}/deps/winonly/COPYING" "winonly copying text"

	write_file "${repo}/go.mod" "module example.com/app

go 1.22

require (
	example.com/alpha v1.2.3
	example.com/winonly v0.4.0
)

replace example.com/alpha => ./deps/alpha

replace example.com/winonly => ./deps/winonly"
	write_file "${repo}/cmd/cairnos/main.go" 'package main

import "example.com/alpha/vendored/inner"

func main() { inner.Use() }'
	write_file "${repo}/cmd/cairnos/main_windows.go" 'package main

import "example.com/winonly/winonly"

func init() { winonly.Use() }'

	printf '%s\n' "$repo"
}

# ─── Running one case ──────────────────────────────────────────────────────

# run_subject runs the copied script from a case's repository with the module
# cache and proxy kept out of reach, leaving its stdout at <repo>/notices.txt
# and its stderr at <repo>/stderr.log.
# Args: repository path, then the targets to pass.
# Returns the script's own exit status.
run_subject() {
	local repo="$1"
	shift
	(
		cd -- "$repo"
		GOFLAGS=-mod=mod GOPROXY=off GOWORK=off GOTOOLCHAIN=local \
			GOMODCACHE="${WORK_DIR}/modcache" \
			sh scripts/third-party-notices.sh "$@" >notices.txt 2>stderr.log
	)
}

# assert_contains fails the case unless the file holds the fixed string.
# Args: case name, file, expected string.
# Returns 0 always; records a failure through fail.
assert_contains() {
	local name="$1" file="$2" needle="$3"
	grep -qF -- "$needle" "$file" || fail "${name}: expected ${file##*/} to contain: ${needle}"
}

# assert_lacks fails the case when the file holds the fixed string.
# Args: case name, file, string that must be absent.
# Returns 0 always; records a failure through fail.
assert_lacks() {
	local name="$1" file="$2" needle="$3"
	if grep -qF -- "$needle" "$file"; then
		fail "${name}: ${file##*/} should not contain: ${needle}"
	fi
}

# assert_status fails the case unless the script exited as expected.
# Args: case name, wanted status, actual status.
# Returns 0 always; records a failure through fail.
assert_status() {
	local name="$1" want="$2" got="$3"
	[[ "$want" == "$got" ]] || fail "${name}: wanted exit status ${want}, got ${got}"
}

# ─── Cases ──────────────────────────────────────────────────────────────────

# case_single_target checks the files one target links: root, nested, and
# NOTICE texts, the Go standard library, and nothing from an unlinked directory,
# a Go source, or a module only another platform links.
# Returns 0 always; records failures through fail.
case_single_target() {
	local name="single_target" repo rc=0
	repo="$(make_repo "$name")"
	run_subject "$repo" linux/amd64 || rc=$?
	assert_status "$name" 0 "$rc"

	local out="${repo}/notices.txt"
	assert_contains "$name" "$out" "Go standard library and runtime go"
	assert_contains "$name" "$out" "The Go Authors"
	assert_contains "$name" "$out" "example.com/alpha v1.2.3"
	assert_contains "$name" "$out" "--- LICENSE ---"
	assert_contains "$name" "$out" "alpha root license text"
	assert_contains "$name" "$out" "--- NOTICE ---"
	assert_contains "$name" "$out" "alpha apache notice text"
	assert_contains "$name" "$out" "--- LICENSE-3RD-PARTY.md ---"
	assert_contains "$name" "$out" "alpha third-party text"
	assert_contains "$name" "$out" "--- vendored/LICENSE ---"
	assert_contains "$name" "$out" "alpha vendored license text"
	assert_lacks "$name" "$out" "alpha unused license text"
	assert_lacks "$name" "$out" "license.go"
	assert_lacks "$name" "$out" "example.com/winonly"
}

# case_union_of_targets checks that a module only windows links shows up once
# windows is among the targets, and that a module every target links gets one
# section rather than one per target.
# Returns 0 always; records failures through fail.
case_union_of_targets() {
	local name="union_of_targets" repo rc=0 sections
	repo="$(make_repo "$name")"
	run_subject "$repo" linux/amd64 windows/amd64 || rc=$?
	assert_status "$name" 0 "$rc"

	local out="${repo}/notices.txt"
	assert_contains "$name" "$out" "example.com/winonly v0.4.0"
	assert_contains "$name" "$out" "winonly copying text"
	sections="$(grep -cxF "example.com/alpha v1.2.3" "$out")" || true
	[[ "$sections" == 1 ]] || fail "${name}: wanted one alpha section, got ${sections}"
}

# case_missing_license checks that a linked module with no license file stops
# the run and names the module, so it cannot ship without its text.
# Returns 0 always; records failures through fail.
case_missing_license() {
	local name="missing_license" repo rc=0
	repo="$(make_repo "$name")"
	rm -- "${repo}/deps/winonly/COPYING"
	run_subject "$repo" windows/amd64 || rc=$?
	assert_status "$name" 1 "$rc"
	assert_contains "$name" "${repo}/stderr.log" "example.com/winonly has no license file"
}

# case_malformed_target checks that a target not shaped goos/goarch is refused
# before go sees it.
# Returns 0 always; records failures through fail.
case_malformed_target() {
	local name="malformed_target" repo rc=0 target
	repo="$(make_repo "$name")"
	for target in linux linux/amd64/v2 "linux/amd 64" /amd64; do
		rc=0
		run_subject "$repo" "$target" || rc=$?
		assert_status "${name} ${target}" 1 "$rc"
		assert_contains "${name} ${target}" "${repo}/stderr.log" "target must look like goos/goarch"
	done
}

# case_host_target checks that a run with no target lists the platform go env
# reports, which is how the Dockerfile calls the script.
# Returns 0 always; records failures through fail.
case_host_target() {
	local name="host_target" repo rc=0 host
	host="$(go env GOOS)/$(go env GOARCH)"
	repo="$(make_repo "$name")"
	run_subject "$repo" || rc=$?
	assert_status "$name" 0 "$rc"

	local out="${repo}/notices.txt"
	assert_contains "$name" "$out" "built for: ${host}"
	assert_contains "$name" "$out" "Go standard library and runtime go"
	assert_contains "$name" "$out" "The Go Authors"
	assert_contains "$name" "$out" "example.com/alpha v1.2.3"
}

# ─── Entry point ────────────────────────────────────────────────────────────

# main runs every case and reports the tally.
# Returns 0 when every case passed; 1 otherwise.
main() {
	local case_name
	command -v go >/dev/null 2>&1 || die "go is required"
	[[ -x "$SUBJECT" ]] || die "not executable: ${SUBJECT}"

	local cases=(
		case_single_target
		case_union_of_targets
		case_missing_license
		case_malformed_target
		case_host_target
	)
	for case_name in "${cases[@]}"; do
		info "running ${case_name}"
		"$case_name"
	done

	[[ "$FAILURES" -eq 0 ]] || die "${FAILURES} expectation(s) failed"
	info "all ${#cases[@]} cases passed"
}

main "$@"
