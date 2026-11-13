#!/bin/sh
# shellcheck disable=SC3043 # dash and busybox ash, the sh on the release runner and in the image builder, both support local
# Print the license and notice texts of everything ./cmd/cairnos links, so a
# release asset or a container image can carry them beside the binary.
#
# MIT, BSD, and ISC need their notice to travel with a binary, and Apache-2.0
# also needs every NOTICE file. The list comes from `go list -deps` at the
# versions go.mod pins, so it is generated at build time and cannot go stale.
# Build tags pick different packages per platform, so each target is listed
# and the union printed.
#
# It covers the Go standard library, the license files at each linked module's
# root, and any license file between a linked package's directory and its
# module root, such as a vendored copy carrying its own LICENSE. A module with
# no license file at all stops the run, so a new dependency cannot ship
# without its text.
#
# Plain sh rather than bash because the Dockerfile runs it in an Alpine
# builder that has no bash.
#
# Usage:
#   scripts/third-party-notices.sh [goos/goarch ...] > THIRD_PARTY_NOTICES.txt
#
# With no target it lists the platform `go env` reports. Run it from the
# module root. Requires: go, find, sort, cut, grep, cat.
set -eu

# Byte order for every sort, so the notices come out identical on any host and
# each module's rows sort together for the final loop, whatever the locale.
LC_ALL=C
export LC_ALL

# info logs a progress line on stderr, which leaves stdout to the notices.
# Returns 0.
info() { echo "I: $*" >&2; }

# die logs why the run cannot go on and stops it. Exits 1, never returns.
die() {
	echo "E: $*" >&2
	exit 1
}

# A tab separates the fields, since module paths and directories hold no tabs
# but can in principle hold spaces.
TAB="$(printf '\t')"
readonly TAB

# The `and` short-circuits, so a standard package's nil Module is never read.
readonly LIST_TEMPLATE="{{if and (not .Standard) (not .Module.Main)}}{{.Module.Path}}${TAB}{{.Module.Version}}${TAB}{{.Module.Dir}}${TAB}{{.Dir}}{{end}}"

# list_packages prints module path, version, module dir, and package dir for
# every non-standard package ./cmd/cairnos links on one goos/goarch target.
# CGO_ENABLED=0 matches how the release and the image build the binary, which
# changes the package set on some platforms.
# Returns 0, or exits through die on a malformed target.
list_packages() {
	local target="$1" goos goarch
	goos="${target%/*}"
	goarch="${target#*/}"
	case "${goos}:${goarch}" in
	:* | *: | *[!a-z0-9:]*) die "target must look like goos/goarch, got '${target}'" ;;
	esac
	[ "${goos}/${goarch}" = "$target" ] || die "target must look like goos/goarch, got '${target}'"
	GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go list -deps -f "$LIST_TEMPLATE" ./cmd/cairnos
}

# license_files prints the license and notice files sitting directly in one
# directory, skipping Go sources such as a license.go. Returns 0.
license_files() {
	find "$1" -maxdepth 1 -type f ! -name '*.go' \( \
		-iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'PATENTS*' \
		\) | sort
}

# module_files prints one row per license file that covers a package: module
# path, version, module dir, and the file's path relative to the module dir.
# It walks from the package directory up to the module root. Returns 0.
module_files() {
	local module="$1" version="$2" module_dir="$3" dir="$4" file
	while :; do
		license_files "$dir" | while IFS= read -r file; do
			printf '%s\t%s\t%s\t%s\n' "$module" "$version" "$module_dir" "${file#"$module_dir"/}"
		done
		case "$dir" in
		"$module_dir"/*) dir="${dir%/*}" ;;
		*) return 0 ;;
		esac
	done
}

# print_text writes one file under a header naming where it came from.
# Returns 0.
print_text() {
	printf -- '--- %s ---\n\n' "$1"
	cat -- "$2"
	printf '\n'
}

# print_module_header opens one module's section. Returns 0.
print_module_header() {
	printf '%s\n%s\n\n' "================================================================================" "$1"
}

# main lists every target, refuses a module that carries no license file, and
# prints the Go standard library texts followed by each module's.
# Returns 0, or exits through die.
main() {
	local target listing packages files module version module_dir file current goroot tool
	for tool in go find sort cut grep cat; do
		command -v "$tool" >/dev/null 2>&1 || die "${tool} is required"
	done
	if [ "$#" -eq 0 ]; then
		set -- "$(go env GOOS)/$(go env GOARCH)"
	fi

	packages=""
	for target in "$@"; do
		info "listing packages for ${target}"
		listing="$(list_packages "$target")"
		packages="${packages}${listing}
"
	done
	packages="$(printf '%s' "$packages" | grep -v '^$' | sort -u)"
	[ -n "$packages" ] || die "go list found no third-party packages"

	files="$(printf '%s\n' "$packages" | while IFS="$TAB" read -r module version module_dir file; do
		module_files "$module" "$version" "$module_dir" "$file"
	done | sort -u)"

	printf '%s\n' "$packages" | cut -f1 | sort -u | while IFS= read -r module; do
		printf '%s\n' "$files" | cut -f1 | grep -qxF -- "$module" ||
			die "${module} has no license file next to its linked packages or at its module root"
	done

	goroot="$(go env GOROOT)"
	printf 'Third-party license and notice texts for cairnos built for: %s\n\n' "$*"
	print_module_header "Go standard library and runtime $(go env GOVERSION)"
	print_text LICENSE "${goroot}/LICENSE"
	if [ -f "${goroot}/PATENTS" ]; then
		print_text PATENTS "${goroot}/PATENTS"
	fi

	current=""
	printf '%s\n' "$files" | while IFS="$TAB" read -r module version module_dir file; do
		if [ "$module" != "$current" ]; then
			print_module_header "${module} ${version}"
			current="$module"
		fi
		print_text "$file" "${module_dir}/${file}"
	done
}

main "$@"
