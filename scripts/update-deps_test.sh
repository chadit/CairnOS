#!/usr/bin/env bash
set -euo pipefail
trap 'echo "E: update-deps_test failed on line $LINENO" >&2' ERR

# update-deps_test.sh - exercise update-deps.sh against stub tools.
#
# Every case copies the real script into a scratch repository and runs it with
# fake go, make, npm, npx, curl, and git first on PATH. The fakes log what they
# were asked to do and serve canned release tags and a canned module graph, so
# the assertions are about the files the script rewrote and the module holds it
# applied, not about the network.
#
# Usage:
#   scripts/update-deps_test.sh
#
# Requires: jq. Touches no network and no repository state.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly SUBJECT="${SCRIPT_DIR}/update-deps.sh"

WORK_DIR="$(mktemp -d)"
readonly WORK_DIR
readonly STUB_DIR="${WORK_DIR}/bin"
readonly DEFAULT_TAGS="${WORK_DIR}/tags.txt"
readonly DEFAULT_GRAPH="${WORK_DIR}/graph.txt"
readonly PG_TAGS="${WORK_DIR}/pg_tags.json"

FAILURES=0

# A step written as `- uses:` and one under a `- name:`, so both rewrite shapes
# get exercised, plus an action whose latest release sits on an older major.
readonly STANDARD_WORKFLOW='name: ci
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - name: Set up Go
        uses: actions/setup-go@v6
      - name: Lint
        uses: golangci/golangci-lint-action@v9'

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

# cleanup removes every scratch repository and stub on any exit path.
# Returns 0.
cleanup() {
	rm -rf -- "$WORK_DIR"
}

trap cleanup EXIT

# ─── Fixtures ───────────────────────────────────────────────────────────────

# write_stubs installs the fake tools every case shares. Each one appends its
# name and arguments to $FAKE_LOG, so a case can check which commands ran.
# Returns 0 on success.
write_stubs() {
	local tool
	mkdir -p -- "$STUB_DIR"

	cat >"${STUB_DIR}/go" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\n' "$*" >>"$FAKE_LOG"
case "$*" in
"list -m -f {{.Version}} github.com/bufbuild/buf") echo v1.73.0 ;;
"list -m -f {{.Version}} modernc.org/sqlite") echo v1.60.1 ;;
"mod graph") cat -- "$FAKE_GO_GRAPH" ;;
esac
STUB

	cat >"${STUB_DIR}/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
url="${*: -1}"
printf 'curl %s\n' "$url" >>"$FAKE_LOG"
case "$url" in
https://hub.docker.com/*)
	cat -- "$FAKE_PG_TAGS"
	;;
https://api.github.com/repos/*/releases/latest)
	repo="${url#https://api.github.com/repos/}"
	repo="${repo%/releases/latest}"
	tag="$(awk -v repo="$repo" '$1 == repo { print $2 }' "$FAKE_TAGS")"
	# 22 is what curl -f exits with on an HTTP error.
	[[ -n "$tag" ]] || exit 22
	printf '{"tag_name": "%s"}\n' "$tag"
	;;
*)
	exit 22
	;;
esac
STUB

	for tool in make npm npx git; do
		cat >"${STUB_DIR}/${tool}" <<'STUB'
#!/usr/bin/env bash
printf '%s %s\n' "${0##*/}" "$*" >>"$FAKE_LOG"
STUB
	done

	chmod +x -- "$STUB_DIR"/*
}

# write_defaults writes the release tags, module graph, and Docker Hub page most
# cases share. The graph holds an older sqlite next to the selected one, so a
# hold that read the wrong node would pin libc to the wrong version.
# Returns 0 on success.
write_defaults() {
	cat >"$DEFAULT_TAGS" <<'TAGS'
actions/checkout v7.1.0
actions/setup-go v7.0.2
golangci/golangci-lint-action v8.0.3
securego/gosec v2.30.1
TAGS

	cat >"$DEFAULT_GRAPH" <<'GRAPH'
github.com/chadit/CairnOS modernc.org/sqlite@v1.60.1
modernc.org/sqlite@v1.59.0 modernc.org/libc@v1.75.7
modernc.org/sqlite@v1.60.1 modernc.org/libc@v1.77.1
github.com/bufbuild/buf@v1.73.0 go.lsp.dev/jsonrpc2@v0.10.0
github.com/bufbuild/buf@v1.73.0 go.lsp.dev/protocol@v0.12.0
github.com/bufbuild/buf@v1.73.0 golang.org/x/mod@v0.41.0
GRAPH

	printf '%s\n' '{"results":[{"name":"18-alpine"},{"name":"18.1-alpine"},{"name":"17-alpine"},{"name":"alpine"}]}' >"$PG_TAGS"
}

# make_repo builds a scratch repository holding the files update-deps.sh reads
# and rewrites. An empty workflow leaves .github/workflows with no file at all.
# Args: case name, workflow text.
# Returns 0 and prints the repository path.
make_repo() {
	local name="$1" workflow="$2"
	local repo="${WORK_DIR}/${name}/repo"

	mkdir -p -- "${repo}/scripts" "${repo}/.github/workflows" "${repo}/tests/integration" "${repo}/web"
	cp -- "$SUBJECT" "${repo}/scripts/update-deps.sh"
	printf 'GOSEC_VERSION := v2.29.0\n' >"${repo}/Makefile"
	printf 'package integration\n\nconst (\n\tpgImage = "postgres:17-alpine"\n)\n' >"${repo}/tests/integration/support.go"
	if [[ -n "$workflow" ]]; then
		printf '%s\n' "$workflow" >"${repo}/.github/workflows/ci.yml"
	fi

	printf '%s\n' "$repo"
}

# ─── Running one case ──────────────────────────────────────────────────────

# run_subject runs the copied script in a case's repository with the stubs first
# on PATH, leaving the tool log at <case dir>/calls.log and the script's stderr
# at <case dir>/stderr.log.
# Args: case name, repository path, and optionally a tags file and a module
#       graph file in place of the defaults.
# Returns the script's own exit status.
run_subject() {
	local name="$1" repo="$2" tags="${3:-$DEFAULT_TAGS}" graph="${4:-$DEFAULT_GRAPH}"
	local case_dir="${WORK_DIR}/${name}"

	FAKE_LOG="${case_dir}/calls.log" FAKE_TAGS="$tags" FAKE_GO_GRAPH="$graph" \
		FAKE_PG_TAGS="$PG_TAGS" PATH="${STUB_DIR}:${PATH}" \
		"${repo}/scripts/update-deps.sh" >/dev/null 2>"${case_dir}/stderr.log"
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

# ─── Cases ──────────────────────────────────────────────────────────────────

# case_moves_pins: a full run moves both action step shapes to the new major,
# keeps an action whose latest release is an older major, rewrites the gosec and
# postgres pins, moves the web lockfile's transitive packages, and holds libc
# and go.lsp.dev where their consumers put them.
# Returns 0 always.
case_moves_pins() {
	local name="moves_pins" repo
	repo="$(make_repo "$name" "$STANDARD_WORKFLOW")"
	run_subject "$name" "$repo" || fail "${name}: the run should succeed"

	local workflow="${repo}/.github/workflows/ci.yml"
	local calls="${WORK_DIR}/${name}/calls.log" stderr="${WORK_DIR}/${name}/stderr.log"
	assert_contains "$name" "$workflow" "      - uses: actions/checkout@v7"
	assert_contains "$name" "$workflow" "        uses: actions/setup-go@v7"
	assert_contains "$name" "$workflow" "        uses: golangci/golangci-lint-action@v9"
	assert_contains "$name" "${repo}/Makefile" "GOSEC_VERSION := v2.30.1"
	assert_contains "$name" "${repo}/tests/integration/support.go" '"postgres:18-alpine"'
	assert_contains "$name" "$calls" "go get -u tool"
	assert_contains "$name" "$calls" "npm update"
	assert_contains "$name" "$calls" "go get modernc.org/libc@v1.77.1"
	assert_contains "$name" "$calls" "go get go.lsp.dev/jsonrpc2@v0.10.0 go.lsp.dev/protocol@v0.12.0"
	assert_lacks "$name" "$calls" "libc@v1.75.7"
	assert_lacks "$name" "$calls" "golang.org/x/mod@"
	assert_contains "$name" "$stderr" "golangci/golangci-lint-action stays at v9"
	assert_lacks "$name" "$stderr" "E:"
}

# case_no_versioned_actions: workflows that name no `owner/repo@vN` are an empty
# list, so the run finishes without an error line.
# Returns 0 always.
case_no_versioned_actions() {
	local name="no_versioned_actions" repo
	repo="$(make_repo "$name" 'jobs:
  test:
    steps:
      - uses: ./.github/actions/local
      - run: make test')"
	run_subject "$name" "$repo" || fail "${name}: the run should succeed"

	local stderr="${WORK_DIR}/${name}/stderr.log"
	assert_contains "$name" "$stderr" "no workflow names a versioned action"
	assert_lacks "$name" "$stderr" "E:"
	assert_contains "$name" "${repo}/tests/integration/support.go" '"postgres:18-alpine"'
}

# case_unreadable_workflows: a workflow scan that cannot read its files stops
# the run before any later pin moves, rather than passing as an empty scan.
# Returns 0 always.
case_unreadable_workflows() {
	local name="unreadable_workflows" repo status=0
	repo="$(make_repo "$name" "")"
	run_subject "$name" "$repo" || status=$?

	[[ "$status" -ne 0 ]] || fail "${name}: the run should exit non-zero"
	assert_contains "$name" "${WORK_DIR}/${name}/stderr.log" "could not read the workflows"
	assert_contains "$name" "${repo}/tests/integration/support.go" '"postgres:17-alpine"'
}

# case_unexpected_tag: a release tag that is not vN.N.N reaches no sed
# expression, and the workflow keeps its reference.
# Returns 0 always.
case_unexpected_tag() {
	local name="unexpected_tag" repo status=0
	local tags="${WORK_DIR}/nightly_tags.txt"
	sed 's|^actions/checkout .*|actions/checkout nightly|' "$DEFAULT_TAGS" >"$tags"
	repo="$(make_repo "$name" "$STANDARD_WORKFLOW")"
	run_subject "$name" "$repo" "$tags" || status=$?

	[[ "$status" -ne 0 ]] || fail "${name}: the run should exit non-zero"
	assert_contains "$name" "${WORK_DIR}/${name}/stderr.log" "actions/checkout published an unexpected tag: nightly"
	assert_contains "$name" "${repo}/.github/workflows/ci.yml" "actions/checkout@v6"
}

# case_stale_hold: when the selected sqlite stops requiring libc, the hold has
# nothing to pin to, and the run says so instead of passing an empty go get.
# Returns 0 always.
case_stale_hold() {
	local name="stale_hold" repo status=0
	local graph="${WORK_DIR}/no_libc_graph.txt"
	grep -v '^modernc.org/sqlite@v1.60.1 ' "$DEFAULT_GRAPH" >"$graph"
	repo="$(make_repo "$name" "$STANDARD_WORKFLOW")"
	run_subject "$name" "$repo" "$DEFAULT_TAGS" "$graph" || status=$?

	[[ "$status" -ne 0 ]] || fail "${name}: the run should exit non-zero"
	assert_contains "$name" "${WORK_DIR}/${name}/stderr.log" "modernc.org/sqlite@v1.60.1 requires nothing under modernc.org/libc@"
}

# ─── Entry point ────────────────────────────────────────────────────────────

# main runs every case and reports the tally.
# Returns 0 when every case passed; 1 otherwise.
main() {
	local case_name
	command -v jq >/dev/null 2>&1 || die "jq is required"
	[[ -x "$SUBJECT" ]] || die "not executable: ${SUBJECT}"
	write_stubs
	write_defaults

	local cases=(
		case_moves_pins
		case_no_versioned_actions
		case_unreadable_workflows
		case_unexpected_tag
		case_stale_hold
	)
	for case_name in "${cases[@]}"; do
		info "running ${case_name}"
		"$case_name"
	done

	[[ "$FAILURES" -eq 0 ]] || die "${FAILURES} expectation(s) failed"
	info "all ${#cases[@]} cases passed"
}

main "$@"
