#!/usr/bin/env bash
# Upgrade everything this repository pins that can be upgraded by a command.
#
# It writes files and nothing else: no commit, no push, no tests. The operator
# reads the diff, runs `make test` and `make lint`, and decides what to keep.
# A bump that breaks the build is a normal outcome of running this.
# -E carries the ERR trap into functions, where every command here runs.
set -Eeuo pipefail

trap 'echo "E: update-deps failed on line $LINENO" >&2' ERR

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

WORKFLOWS_DIR="$REPO_ROOT/.github/workflows"
readonly WORKFLOWS_DIR

PG_IMAGE_FILE="$REPO_ROOT/tests/integration/support.go"
readonly PG_IMAGE_FILE

# WORK holds a rewritten file while sed produces it, and is cleared once the
# rewrite lands. It is a file rather than a directory so the EXIT trap has one
# thing to remove, and it sits beside its target so replacing it is a rename on
# the same filesystem rather than a truncate and copy.
WORK=""

# info logs a progress line on stderr, which leaves stdout to the git status
# report at the end. Returns 0.
info() { echo "I: $*" >&2; }

# die logs why the run cannot go on and stops it. Exits 1, never returns.
die() {
	echo "E: $*" >&2
	exit 1
}

# cleanup removes the scratch file on every exit path, including a failure part
# way through a rewrite. Returns 0.
cleanup() {
	if [[ -n "$WORK" ]]; then
		rm -f -- "$WORK"
	fi
}

trap cleanup EXIT

# require_tools stops before the first upgrade when something is missing, so a
# run never leaves half the pins moved and half untouched. Returns 0 or dies.
require_tools() {
	local tool
	for tool in go make npm npx curl jq git; do
		command -v "$tool" >/dev/null 2>&1 || die "$tool is required and is not on PATH"
	done
}

# pin_to_requirements sets each requirement of module $1 that starts with $2
# to the version $1's selected release names, undoing what `go get -u` did to
# it. $2 is a module path ending in @ or a path prefix ending in /. Matching
# nothing dies, so a hold nobody needs gets noticed and dropped. Returns 0 or
# dies.
pin_to_requirements() {
	local requirer="$1" prefix="$2" version requirements requirement
	local -a pins=()

	version="$(go list -m -f '{{.Version}}' "$requirer")"
	requirements="$(go mod graph | awk -v node="${requirer}@${version}" -v prefix="$prefix" \
		'$1 == node && index($2, prefix) == 1 { print $2 }')"
	[[ -n "$requirements" ]] || die "${requirer}@${version} requires nothing under ${prefix}, so its hold can go"

	while IFS= read -r requirement; do
		pins+=("$requirement")
	done <<<"$requirements"

	go get "${pins[@]}"
	info "held ${pins[*]} at what ${requirer}@${version} requires"
}

# update_go_modules moves the module graph, the tool directives, and the
# toolchain line. The tool directives need their own command, since nothing
# under ./... imports buf or the protoc plugins. Returns 0 or dies.
update_go_modules() {
	info "upgrading Go module dependencies"
	go get -u -t ./...

	# -u moves the tools' own dependencies too, so a security fix in a module
	# only buf or a protoc plugin uses still arrives through this script.
	info "upgrading Go tool dependencies"
	go get -u tool

	# The -u steps move libc and go.lsp.dev past what sqlite and buf depend on.
	# modernc.org/sqlite tells downstream modules to require exactly the libc
	# its own go.mod lists. buf 1.73 stopped building once go.lsp.dev/protocol
	# reached v1, and jsonrpc2 and uri move with it, so the whole go.lsp.dev
	# family stays where buf's go.mod puts it.
	pin_to_requirements modernc.org/sqlite modernc.org/libc@
	pin_to_requirements github.com/bufbuild/buf go.lsp.dev/

	info "upgrading the Go toolchain line"
	go get go@latest

	info "tidying go.mod and go.sum"
	go mod tidy
}

# update_buf refreshes the registry dependency in buf.lock and regenerates, since
# a googleapis bump can change what comes out of the plugins. Returns 0 or dies.
update_buf() {
	info "upgrading buf registry dependencies"
	go tool buf dep update

	info "regenerating protobuf code"
	make proto
}

# update_web rewrites web/package.json to the newest versions, then moves the
# packages those pull in. Each package.json entry is an exact version, so ncu
# has to move it; `npm update` can't. The lockfile's transitive packages move
# only when `npm update` lifts them as far as their dependants accept, so a
# fix deep in the tree arrives. Returns 0 or dies.
update_web() {
	info "upgrading web dependencies"
	(
		cd "$REPO_ROOT/web" || die "cannot enter $REPO_ROOT/web"
		# --peer holds a package at the newest version its dependants accept;
		# typescript-eslint caps typescript, and npm install refuses the tree past it.
		npx --yes npm-check-updates -u --peer
		npm install
		npm update
	)
}

# latest_tag prints the newest release tag of a GitHub repository, or `null`
# when it has none. The API allows sixty unauthenticated calls an hour. This
# script makes one per action the workflows use plus one for gosec, and the
# Bitwarden pin refresh reads one to five release pages. Returns non-zero when
# the request fails.
latest_tag() {
	local repo="$1"
	curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" | jq -r '.tag_name'
}

# rewrite_file applies one sed expression to a file. The scratch file starts as
# a copy so it carries the target's own mode, and lands by rename, so an
# interrupted run leaves the previous file whole. Returns 0 or dies.
rewrite_file() {
	local file="$1" expression="$2" rewritten

	rewritten="$(mktemp "${file}.XXXXXX")"
	WORK="$rewritten"

	cp -p -- "$file" "$rewritten"
	sed "$expression" "$file" >"$rewritten"
	mv -- "$rewritten" "$file"

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

	rewrite_file "$REPO_ROOT/Makefile" "s|^\(${variable} *\):= *.*|\1:= ${tag}|"

	info "$variable $current -> $tag"
}

# update_gosec moves the gosec pin the Makefile holds. golangci-lint has no
# pin to move: the Makefile and CI both install @latest. Returns 0 or dies.
update_gosec() {
	info "checking the gosec pin"
	pin_version GOSEC_VERSION securego/gosec
}

# workflow_actions prints each distinct `owner/repo@vN` the workflows use, one
# per line, so an action three jobs share costs one API call and one rewrite.
# awk rather than grep, since grep exits 1 when nothing matches, and workflows
# with no versioned action are an empty list rather than an error. Returns
# non-zero only when a workflow file can't be read.
workflow_actions() {
	awk 'match($0, /uses: [A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+@v[0-9]+$/) {
		print substr($0, RSTART + 6, RLENGTH - 6)
	}' "$WORKFLOWS_DIR"/*.yml | sort -u
}

# update_action moves one GitHub Action to the major of its newest release.
# Workflows name a major, which the action's maintainers move as they release,
# so only the major is rewritten and a newer release on the same major changes
# nothing. Returns 0 or dies.
update_action() {
	local action="$1"
	local repo="${action%@*}" current="${action#*@}" tag major file

	tag="$(latest_tag "$repo")"
	[[ "$tag" =~ ^v[0-9]+(\.[0-9]+)*$ ]] || die "$repo published an unexpected tag: $tag"
	major="${tag%%.*}"

	# Compared as numbers, since a maintainer can mark a backport on an older
	# major as the latest release, and that must not move a workflow backwards.
	if ((10#${major#v} <= 10#${current#v})); then
		info "$repo stays at $current, its latest release is $tag"
		return
	fi

	# A step written as `- uses:` and one under a `- name:` both match. The
	# optional group is \{0,1\} because BSD sed reads \? as a literal.
	for file in "$WORKFLOWS_DIR"/*.yml; do
		rewrite_file "$file" "s|^\( *\(- \)\{0,1\}uses: ${repo}\)@${current}$|\1@${major}|"
	done

	if grep -q "uses: ${action}$" "$WORKFLOWS_DIR"/*.yml; then
		die "a ${action} reference survived the rewrite"
	fi

	info "$repo $current -> $major"
}

# update_actions walks every action the workflows reference. Dependabot would
# do this too, but it opens one pull request per action, and this keeps a
# dependency bump to one branch. The list is captured before the loop because
# bash drops a process substitution's exit status, so a scan that failed would
# pass as one that found nothing. Returns 0 or dies.
update_actions() {
	info "checking GitHub Action versions"
	local actions action

	actions="$(workflow_actions)" || die "could not read the workflows in $WORKFLOWS_DIR"
	if [[ -z "$actions" ]]; then
		info "no workflow names a versioned action"
		return
	fi

	while IFS= read -r action; do
		update_action "$action"
	done <<<"$actions"
}

# update_postgres moves the image the integration tests start to the newest
# alpine-tagged major. Docker Hub lists tags newest-updated first and rebuilds
# every supported major together, so one page holds them all. The Dockerfile's
# FROM lines stay: the builder floats on purpose, and the runtime's Debian
# major is a decision made by hand, not a bump. Returns 0 or dies.
update_postgres() {
	info "checking the postgres image the integration tests start"
	local current major

	current="$(sed -n 's/^.*pgImage *= *"postgres:\([0-9]*\)-alpine".*$/\1/p' "$PG_IMAGE_FILE")"
	[[ "$current" =~ ^[0-9]+$ ]] || die "pgImage in $PG_IMAGE_FILE is not a postgres:<major>-alpine tag"

	major="$(curl -fsSL 'https://hub.docker.com/v2/repositories/library/postgres/tags?page_size=100&name=-alpine' |
		jq -r '[.results[].name | capture("^(?<major>[0-9]+)-alpine$").major | tonumber] | max')"
	[[ "$major" =~ ^[0-9]+$ ]] || die "could not read the postgres tags from Docker Hub"

	if [[ "$major" == "$current" ]]; then
		info "pgImage is already postgres:${current}-alpine"
		return
	fi

	rewrite_file "$PG_IMAGE_FILE" "s|postgres:${current}-alpine|postgres:${major}-alpine|"

	info "pgImage postgres:${current}-alpine -> postgres:${major}-alpine"
}

# update_bitwarden refreshes the pinned Bitwarden CLI. Upstream publishes no
# checksum file, so a bump downloads both platform archives and hashes them,
# which is around 90 MB. A pin that is already newest downloads nothing.
# Returns 0 or dies.
update_bitwarden() {
	info "checking the Bitwarden CLI pin"
	go run ./tools/bwpin
}

# report_next prints the working tree and the two commands that decide whether
# any of this is keepable. Nothing here is committed: a dependency bump is a
# change to review, not one to trust. Returns 0 or dies.
report_next() {
	info "changed files:"
	git status --short

	info "run 'make test' and 'make lint' before committing"
}

# main runs every upgrade in order, Go first so the buf and Bitwarden steps
# build against the new module graph. Returns 0 or dies.
main() {
	cd "$REPO_ROOT" || die "cannot enter $REPO_ROOT"

	require_tools
	update_go_modules
	update_buf
	update_web
	update_gosec
	update_actions
	update_postgres
	update_bitwarden
	report_next
}

main "$@"
