#!/usr/bin/env bash
# Upgrade everything this repository pins that can be upgraded by a command.
#
# It writes files and nothing else: no commit, no push, no tests. The operator
# reads the diff, runs `make test` and `make lint`, and decides what to keep.
# A bump that breaks the build is a normal outcome of running this.
set -euo pipefail

trap 'echo "E: update-deps failed on line $LINENO" >&2' ERR

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

# WORK holds the rewritten Makefile while sed produces it, and is cleared once
# the rewrite lands. It is a file rather than a directory so the EXIT trap has
# one thing to remove, and it sits beside the Makefile so replacing it is a
# rename on the same filesystem rather than a truncate and copy.
WORK=""

info() { echo "I: $*" >&2; }

die() {
	echo "E: $*" >&2
	exit 1
}

# cleanup removes the scratch file on every exit path, including a failure part
# way through a Makefile rewrite.
cleanup() {
	if [[ -n "$WORK" ]]; then
		rm -f -- "$WORK"
	fi
}

trap cleanup EXIT

# require_tools stops before the first upgrade when something is missing, so a
# run never leaves half the pins moved and half untouched.
require_tools() {
	local tool
	for tool in go make npm npx curl jq git; do
		command -v "$tool" >/dev/null 2>&1 || die "$tool is required and is not on PATH"
	done
}

# update_go_modules moves the module graph, the tool directives, and the
# toolchain line. The tool directives need their own command: tool packages are
# not imported by anything under ./..., so `go get -u ./...` never reaches buf
# or the protoc plugins.
update_go_modules() {
	info "upgrading Go module dependencies"
	go get -u -t ./...

	info "upgrading Go tool dependencies"
	go get -u tool

	info "upgrading the Go toolchain line"
	go get go@latest

	info "tidying go.mod and go.sum"
	go mod tidy
}

# update_buf refreshes the registry dependency in buf.lock and regenerates, since
# a googleapis bump can change what comes out of the plugins.
update_buf() {
	info "upgrading buf registry dependencies"
	go tool buf dep update

	info "regenerating protobuf code"
	make proto
}

# update_web rewrites web/package.json to the newest versions. `npm update` is a
# no-op here: every dependency is an exact version, which is a semver range of
# one, so nothing is ever in range to move to.
update_web() {
	info "upgrading web dependencies"
	(
		cd "$REPO_ROOT/web" || die "cannot enter $REPO_ROOT/web"
		npx --yes npm-check-updates -u
		npm install
	)
}

# latest_tag prints the newest release tag of a GitHub repository. The API
# allows sixty unauthenticated calls an hour, which this script uses three of.
latest_tag() {
	local repo="$1"
	curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" | jq -r '.tag_name'
}

# rewrite_pin replaces one variable's value in the Makefile. The scratch file
# starts as a copy so it carries the Makefile's own mode, and lands by rename,
# so an interrupted run leaves the previous Makefile whole. Returns 0 or dies.
rewrite_pin() {
	local variable="$1" tag="$2" rewritten

	rewritten="$(mktemp "${REPO_ROOT}/Makefile.XXXXXX")"
	WORK="$rewritten"

	cp -p -- "$REPO_ROOT/Makefile" "$rewritten"
	sed "s|^\(${variable} *\):= *.*|\1:= ${tag}|" "$REPO_ROOT/Makefile" >"$rewritten"
	mv -- "$rewritten" "$REPO_ROOT/Makefile"

	WORK=""
}

# pin_version rewrites one Makefile version variable to a repository's newest
# release tag. A missing variable is a failure rather than an append: a silent
# no-op would leave the linter frozen while everything else moved. The tag is
# checked because it reaches a sed replacement, where &, | and a backslash each
# mean something. Returns 0 or dies.
pin_version() {
	local variable="$1" repo="$2"
	local current tag

	current="$(sed -n "s/^${variable} *:= *//p" "$REPO_ROOT/Makefile")"
	[[ -n "$current" ]] || die "$variable is not set in $REPO_ROOT/Makefile"

	tag="$(latest_tag "$repo")"
	[[ -n "$tag" && "$tag" != "null" ]] || die "could not read the newest release of $repo"

	[[ "$tag" =~ ^v?[0-9]+(\.[0-9]+)*$ ]] || die "$repo published an unexpected tag: $tag"

	if [[ "$tag" == "$current" ]]; then
		info "$variable is already $current"
		return
	fi

	rewrite_pin "$variable" "$tag"

	info "$variable $current -> $tag"
}

# update_linters moves the two pins the Makefile holds and CI reads.
update_linters() {
	info "checking the linter pins"
	pin_version GOLANGCI_LINT_VERSION golangci/golangci-lint
	pin_version GOSEC_VERSION securego/gosec
}

# update_bitwarden refreshes the pinned Bitwarden CLI. Upstream publishes no
# checksum file, so a bump downloads both platform archives and hashes them,
# which is around 90 MB. A pin that is already newest downloads nothing.
update_bitwarden() {
	info "checking the Bitwarden CLI pin"
	go run ./tools/bwpin
}

# report_manual names what this script deliberately leaves alone, so the list
# does not have to be remembered.
report_manual() {
	info "left alone, bump by hand:"
	info "  Dockerfile: both FROM lines carry deliberate float markers"
	info "  tests/integration/support.go: the postgres image tag"
	info "  .github/workflows/*.yml: action versions"
}

# report_next prints the working tree and the two commands that decide whether
# any of this is keepable. Nothing here is committed: a dependency bump is a
# change to review, not one to trust.
report_next() {
	info "changed files:"
	git status --short

	info "run 'make test' and 'make lint' before committing"
}

main() {
	cd "$REPO_ROOT" || die "cannot enter $REPO_ROOT"

	require_tools
	update_go_modules
	update_buf
	update_web
	update_linters
	update_bitwarden
	report_manual
	report_next
}

main "$@"
