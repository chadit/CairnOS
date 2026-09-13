#!/usr/bin/env bash
set -euo pipefail
trap 'echo "E: model-update-issues_test failed on line $LINENO" >&2' ERR

# model-update-issues_test.sh - exercise model-update-issues.sh against a stub gh.
#
# Every case runs the real script with a fake `gh` first on PATH. The fake logs
# what it was asked to do and serves a canned issue list, so the assertions are
# about the decisions the script made, not about GitHub.
#
# Usage:
#   scripts/model-update-issues_test.sh
#
# Requires: jq. Touches no network and no repository state.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly SUBJECT="${SCRIPT_DIR}/model-update-issues.sh"

WORK_DIR="$(mktemp -d)"
readonly WORK_DIR
trap 'rm -rf -- "$WORK_DIR"' EXIT

FAILURES=0

# ─── Logging ────────────────────────────────────────────────────────────────

info() { echo "I: $*" >&2; }
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

# ─── Fixtures ───────────────────────────────────────────────────────────────

# write_fake_gh installs the stub gh into the test bin directory.
# Returns 0 on success.
write_fake_gh() {
	local bin_dir="${WORK_DIR}/bin"
	mkdir -p -- "$bin_dir"
	cat >"${bin_dir}/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_GH_LOG"
if [[ -n "${FAKE_GH_FAIL:-}" && "$*" == *"${FAKE_GH_FAIL}"* ]]; then
	echo "fake gh: refusing ${FAKE_GH_FAIL}" >&2
	exit 1
fi
case "${1:-} ${2:-}" in
"issue list")
	cat -- "$FAKE_GH_ISSUES"
	;;
"issue create")
	next="$(($(cat -- "$FAKE_GH_COUNTER") + 1))"
	printf '%s\n' "$next" >"$FAKE_GH_COUNTER"
	printf 'https://github.com/chadit/CairnOS/issues/%s\n' "$next"
	;;
*) ;;
esac
STUB
	chmod +x "${bin_dir}/gh"
}

# hex_fill prints one hex digit repeated to the requested width, so a fixture
# can name a distinct revision or set of weights in one character.
# Args: digit, width.
# Returns 0 and prints the string.
hex_fill() {
	local digit="$1" width="$2" zeros
	zeros="$(printf "%0${width}d" 0)"
	printf '%s' "${zeros//0/$digit}"
}

# write_report writes a check report. The model's revision and weight hash are
# named by separate digits because the two move independently, which is the
# whole reason the title is keyed on the hash.
# Args: path, runtime latest version, runtime outdated flag, model sha256
#       digit, model outdated flag, model revision digit.
# Returns 0 on success.
write_report() {
	local path="$1" runtime_latest="$2" runtime_outdated="$3"
	local model_sha_digit="$4" model_outdated="$5" model_rev_digit="$6"
	local model_latest model_sha
	model_latest="$(hex_fill "$model_rev_digit" 40)"
	model_sha="$(hex_fill "$model_sha_digit" 64)"
	cat >"$path" <<JSON
{
  "runtime": {
    "name": "llama.cpp",
    "pinned": "v0.4.0",
    "pinned_build": "b10809",
    "latest": "${runtime_latest}",
    "latest_build": "b11002",
    "outdated": ${runtime_outdated},
    "url": "https://github.com/ggml-org/llama.cpp/releases/tag/${runtime_latest}"
  },
  "models": [
    {
      "name": "gemma-4-e4b",
      "repo": "ggml-org/gemma-4-E4B-it-GGUF",
      "file": "gemma-4-E4B-it-Q4_0.gguf",
      "pinned_revision": "1111111111111111111111111111111111111111",
      "latest_revision": "${model_latest}",
      "pinned_sha256": "$(hex_fill 1 64)",
      "latest_sha256": "${model_sha}",
      "outdated": ${model_outdated},
      "url": "https://huggingface.co/ggml-org/gemma-4-E4B-it-GGUF/commits/main"
    }
  ]
}
JSON
}

# ─── Running one case ──────────────────────────────────────────────────────

# run_subject runs the script under test in a clean case directory, leaving the
# gh call log at <case dir>/gh.log and its stderr at <case dir>/stderr.log.
# Args: case name, report path, issues array JSON, starting issue number, and
#       optionally a gh argument substring the stub should refuse.
# Returns the script's own exit status.
run_subject() {
	local name="$1" report="$2" issues="$3" start="$4" fail_on="${5:-}"
	local case_dir="${WORK_DIR}/${name}"
	mkdir -p -- "$case_dir"
	printf '%s\n' "$issues" >"${case_dir}/issues.json"
	printf '%s\n' "$start" >"${case_dir}/counter"
	: >"${case_dir}/gh.log"
	PATH="${WORK_DIR}/bin:${PATH}" \
		FAKE_GH_FAIL="$fail_on" \
		FAKE_GH_LOG="${case_dir}/gh.log" \
		FAKE_GH_ISSUES="${case_dir}/issues.json" \
		FAKE_GH_COUNTER="${case_dir}/counter" \
		"$SUBJECT" "$report" 2>"${case_dir}/stderr.log"
}

# assert_logged fails the run unless the gh log holds a line with the substring.
# Args: case name, expected substring.
# Returns 0 always; records a failure through fail.
assert_logged() {
	local name="$1" needle="$2"
	grep -qF -- "$needle" "${WORK_DIR}/${name}/gh.log" ||
		fail "${name}: expected a gh call matching: ${needle}"
}

# assert_not_logged fails the run when the gh log holds the substring.
# Args: case name, substring that must be absent.
# Returns 0 always; records a failure through fail.
assert_not_logged() {
	local name="$1" needle="$2"
	grep -qF -- "$needle" "${WORK_DIR}/${name}/gh.log" &&
		fail "${name}: unexpected gh call matching: ${needle}"
	return 0
}

# assert_stderr fails the run unless the script's own stderr holds the substring.
# Args: case name, expected substring.
# Returns 0 always; records a failure through fail.
assert_stderr() {
	local name="$1" needle="$2"
	grep -qF -- "$needle" "${WORK_DIR}/${name}/stderr.log" ||
		fail "${name}: expected a log line matching: ${needle}"
}

# foreign_issues prints an issue array of the requested size whose titles all
# belong to a different component, so a search can fill its page without giving
# the script anything it is allowed to touch.
# Args: count.
# Returns 0 and prints the array.
foreign_issues() {
	local count="$1"
	jq -nc --argjson count "$count" '[range(0; $count) | {
		number: (1000 + .),
		title: ("model-update: other-model " + (. | tostring)),
		state: "CLOSED"
	}]'
}

# ─── Cases ──────────────────────────────────────────────────────────────────

# case_create_when_missing: nothing tracked yet, so both components get an issue
# and nothing gets closed.
# Returns 0 always.
case_create_when_missing() {
	local name="create_when_missing" report="${WORK_DIR}/create.json"
	write_report "$report" "v0.5.0" true 2 true a
	run_subject "$name" "$report" "[]" 100
	assert_logged "$name" "issue create --title model-update: llama.cpp v0.5.0"
	assert_logged "$name" "issue create --title model-update: gemma-4-e4b 222222222222"
	assert_not_logged "$name" "issue close"
}

# case_skip_when_closed: a closed issue for the exact version is a decision
# someone already made, so the script must not open a second one.
# Returns 0 always.
case_skip_when_closed() {
	local name="skip_when_closed" report="${WORK_DIR}/skip.json" issues
	write_report "$report" "v0.5.0" true 3 false a
	issues='[{"number":5,"title":"model-update: llama.cpp v0.5.0","state":"CLOSED"}]'
	run_subject "$name" "$report" "$issues" 100
	assert_not_logged "$name" "issue create --title model-update: llama.cpp"
	assert_not_logged "$name" "issue close 5"
}

# case_supersede_older: an open issue for an earlier version is closed against
# the issue that replaces it.
# Returns 0 always.
case_supersede_older() {
	local name="supersede_older" report="${WORK_DIR}/supersede.json" issues
	write_report "$report" "v0.5.0" true 4 false a
	issues='[{"number":7,"title":"model-update: llama.cpp v0.4.5","state":"OPEN"}]'
	run_subject "$name" "$report" "$issues" 100
	assert_logged "$name" "issue create --title model-update: llama.cpp v0.5.0"
	assert_logged "$name" "issue comment 7 --body Superseded by #101"
	assert_logged "$name" "issue close 7 --reason not planned"
}

# case_close_when_caught_up: the pin matches upstream, so every open issue for
# that component is closed as done rather than as declined.
# Returns 0 always.
case_close_when_caught_up() {
	local name="close_when_caught_up" report="${WORK_DIR}/caught.json" issues
	write_report "$report" "v0.5.0" false 5 false a
	issues='[{"number":9,"title":"model-update: llama.cpp v0.4.9","state":"OPEN"}]'
	run_subject "$name" "$report" "$issues" 100
	assert_logged "$name" "issue comment 9 --body Pinned at v0.5.0"
	assert_logged "$name" "issue close 9 --reason completed"
	assert_not_logged "$name" "issue create"
}

# case_idempotent: a second run over the same report, with the issues the first
# run opened now present and open, must write nothing at all.
# Returns 0 always.
case_idempotent() {
	local name="idempotent" report="${WORK_DIR}/idempotent.json" issues
	write_report "$report" "v0.5.0" true 6 true a
	run_subject "first_pass" "$report" "[]" 100
	issues='[{"number":101,"title":"model-update: llama.cpp v0.5.0","state":"OPEN"},'
	issues+='{"number":102,"title":"model-update: gemma-4-e4b 666666666666","state":"OPEN"}]'
	run_subject "$name" "$report" "$issues" 200
	assert_not_logged "$name" "issue create"
	assert_not_logged "$name" "issue close"
	assert_not_logged "$name" "issue comment"
}

# case_revision_moved: a README-only commit on the model repo moves the revision
# and leaves the weights alone. The issue already tracking those weights must
# survive it, rather than being superseded by a second issue for the same file.
# Returns 0 always.
case_revision_moved() {
	local name="revision_moved" report="${WORK_DIR}/revision.json" issues
	write_report "$report" "v0.5.0" false 7 true b
	issues='[{"number":55,"title":"model-update: gemma-4-e4b 777777777777","state":"OPEN"}]'
	run_subject "$name" "$report" "$issues" 100
	assert_not_logged "$name" "issue create"
	assert_not_logged "$name" "issue close 55"
	assert_not_logged "$name" "issue comment 55"
}

# case_search_limit_warns: a full page of results means older issues were cut
# off, and a stale one below the cut would never be closed. The run continues,
# so the warning is the only thing that makes the gap visible.
# Returns 0 always.
case_search_limit_warns() {
	local name="search_limit_warns" report="${WORK_DIR}/limit.json"
	write_report "$report" "v0.5.0" false 8 false a
	run_subject "$name" "$report" "$(foreign_issues 100)" 100
	assert_stderr "$name" "search returned 100 issues"
}

# case_gh_failure: a gh call that fails has to stop the run. Carrying on would
# leave the issue set half reconciled and report success for it.
# Returns 0 always.
case_gh_failure() {
	local name="gh_failure" report="${WORK_DIR}/ghfail.json" status
	write_report "$report" "v0.5.0" true 9 false a
	status=0
	run_subject "$name" "$report" "[]" 100 "issue create" || status=$?
	[[ "$status" -ne 0 ]] || fail "${name}: a failing gh call should exit non-zero"
}

# case_prefix_not_fuzzy: GitHub's in:title search matches loosely, so a sibling
# component whose name extends this one comes back in the results. The prefix
# filter has to drop it, or one model's run would close another model's issues.
# Returns 0 always.
case_prefix_not_fuzzy() {
	local name="prefix_not_fuzzy" report="${WORK_DIR}/fuzzy.json" issues
	write_report "$report" "v0.5.0" false 8 true a
	issues='[{"number":31,"title":"model-update: gemma-4-e4b-base abc123def456","state":"OPEN"}]'
	run_subject "$name" "$report" "$issues" 100
	assert_logged "$name" "issue create --title model-update: gemma-4-e4b 888888888888"
	assert_not_logged "$name" "issue close 31"
	assert_not_logged "$name" "issue comment 31"
}

# case_bad_report: a missing file and a report without .models both have to stop
# the run, because filing issues off a half-written report is worse than not
# filing them.
# Returns 0 always.
case_bad_report() {
	local name="bad_report" report="${WORK_DIR}/bad.json" status
	printf '%s\n' '{"runtime":{"name":"llama.cpp"}}' >"$report"
	status=0
	run_subject "$name" "$report" "[]" 100 || status=$?
	[[ "$status" -ne 0 ]] || fail "${name}: a report without .models should exit non-zero"

	status=0
	run_subject "missing_report" "${WORK_DIR}/absent.json" "[]" 100 || status=$?
	[[ "$status" -ne 0 ]] || fail "missing_report: an absent report should exit non-zero"
}

# ─── Entry point ────────────────────────────────────────────────────────────

# main runs every case and reports the tally.
# Returns 0 when every case passed; 1 otherwise.
main() {
	local case_name
	command -v jq >/dev/null 2>&1 || die "jq is required"
	[[ -x "$SUBJECT" ]] || die "not executable: ${SUBJECT}"
	write_fake_gh

	local cases=(
		case_create_when_missing
		case_skip_when_closed
		case_supersede_older
		case_close_when_caught_up
		case_idempotent
		case_revision_moved
		case_search_limit_warns
		case_gh_failure
		case_prefix_not_fuzzy
		case_bad_report
	)
	for case_name in "${cases[@]}"; do
		info "running ${case_name}"
		"$case_name"
	done

	[[ "$FAILURES" -eq 0 ]] || die "${FAILURES} expectation(s) failed"
	info "all ${#cases[@]} cases passed"
}

main "$@"
