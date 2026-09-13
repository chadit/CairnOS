#!/usr/bin/env bash
set -euo pipefail
trap 'echo "E: model-update-issues failed on line $LINENO" >&2' ERR

# model-update-issues.sh - turn `cairnos model check --json` output into issues.
#
# Reads the JSON report produced by the model check and keeps one tracking issue
# per outdated component: the llama.cpp runtime, and every pinned model. An
# issue closed by hand stays closed, because closing one is how a human says the
# bump was considered and declined.
#
# Usage:
#   scripts/model-update-issues.sh <check.json>
#
# Requires: gh authenticated for the target repository, plus jq.
# Honors GH_TOKEN and GH_REPO the way every other gh call does.

# Label carried by every issue this script owns. Nothing else may use it: the
# search below treats the label as the boundary of what the script may close.
readonly ISSUE_LABEL="model-update"

# Title prefix shared by every issue. The component key and version follow it,
# which is what makes an exact title the identity of one upstream version.
readonly TITLE_STEM="model-update:"

# The same sentence goes in every body. Whoever picks the issue up needs the
# command, not a description of the situation they already read above it.
readonly FIX_LINE="Fix: bump the pin in internal/localmodel and run cairnos model upgrade"

# How many issues one component's search may return. A component accumulates one
# issue per upstream release, so a hundred is years of history.
readonly SEARCH_LIMIT=100

# ─── Logging ────────────────────────────────────────────────────────────────

# info reports progress. Returns 0.
info() { echo "I: $*" >&2; }

# warn reports something the run survived but a reader should see. Returns 0.
warn() { echo "W: $*" >&2; }

# die reports a condition the run cannot continue past. Exits 1, never returns.
die() {
	echo "E: $*" >&2
	exit 1
}

# ─── Preconditions ──────────────────────────────────────────────────────────

# require_tools aborts unless every external command this script calls exists.
# Returns 0 when all are present; exits 1 through die otherwise.
require_tools() {
	local tool
	for tool in gh jq; do
		command -v "$tool" >/dev/null 2>&1 || die "${tool} is required"
	done
}

# validate_input checks that the report path was given, exists, and carries both
# top-level keys the rest of the script reads.
# Returns 0 when the report is usable; exits 1 through die otherwise.
validate_input() {
	local input="$1"
	[[ -n "$input" ]] || die "usage: model-update-issues.sh <check.json>"
	[[ -f "$input" ]] || die "no such report: ${input}"
	jq -e '.runtime and .models' -- "$input" >/dev/null ||
		die "report is missing .runtime or .models: ${input}"
}

# ensure_label creates the tracking label, or refreshes it when it already
# exists, so a fresh repository does not fail the first issue creation.
# Returns 0 on success; a gh failure trips the ERR trap and set -e.
ensure_label() {
	gh label create "$ISSUE_LABEL" --force \
		--color "1d76db" \
		--description "pinned runtime or model is behind upstream" >/dev/null
}

# ─── Issue lookup ───────────────────────────────────────────────────────────

# list_component_issues returns the issues belonging to one component as a JSON
# array of {number, title, state}. GitHub's in:title search is fuzzy, so the
# result is filtered again here on an exact prefix; the search only narrows what
# has to be fetched.
# Args: title prefix, including its trailing space.
# Returns 0 and prints the array.
list_component_issues() {
	local prefix="$1" raw filtered
	raw="$(gh issue list \
		--label "$ISSUE_LABEL" \
		--state all \
		--search "in:title \"${prefix}\"" \
		--json number,title,state \
		--limit "$SEARCH_LIMIT")" || die "gh issue list failed for: ${prefix}"
	# A full page means the search hit the limit and older issues were cut off,
	# so a stale one further down would never get closed.
	[[ "$(jq -r 'length' <<<"$raw")" -lt "$SEARCH_LIMIT" ]] ||
		warn "${prefix%% } search returned ${SEARCH_LIMIT} issues; older ones were not examined"
	filtered="$(jq -c --arg prefix "$prefix" \
		'[.[] | select(.title | startswith($prefix))]' <<<"$raw")" ||
		die "could not parse the issue list for: ${prefix}"
	printf '%s\n' "$filtered"
}

# issue_number_for_title prints the number of the issue whose title matches
# exactly, in any state, or nothing when there is none.
# Args: issue array JSON, exact title.
# Returns 0 either way; an empty line means no match.
issue_number_for_title() {
	local issues="$1" title="$2"
	jq -r --arg title "$title" \
		'[.[] | select(.title == $title) | .number] | first // empty' <<<"$issues"
}

# open_rows prints the open issues from an issue array as number and title on
# one tab-separated line each, which is what the close loops iterate.
# Args: issue array JSON.
# Returns 0; prints nothing when no issue is open.
open_rows() {
	local issues="$1"
	jq -r '.[] | select(.state == "OPEN") | [(.number | tostring), .title] | @tsv' <<<"$issues"
}

# ─── Issue writes ───────────────────────────────────────────────────────────

# create_issue opens one tracking issue and prints its number. gh prints the new
# issue URL, whose last path segment is the number.
# Args: title, body.
# Returns 0 and prints the number; a gh failure trips the ERR trap.
create_issue() {
	local title="$1" body="$2" url
	url="$(gh issue create --title "$title" --body "$body" --label "$ISSUE_LABEL")" ||
		die "gh issue create failed for: ${title}"
	[[ -n "$url" ]] || die "gh issue create printed no URL for: ${title}"
	printf '%s\n' "${url##*/}"
}

# supersede_older closes every open issue for this component that tracks some
# other version, pointing each at the issue for the current one.
# Args: issue array JSON, title prefix, current version, replacement number.
# Returns 0; a gh failure trips the ERR trap.
supersede_older() {
	local issues="$1" prefix="$2" version="$3" replacement="$4"
	local number title tracked
	while IFS=$'\t' read -r number title; do
		[[ -n "$number" ]] || continue
		tracked="${title#"$prefix"}"
		[[ "$tracked" != "$version" ]] || continue
		gh issue comment "$number" --body "Superseded by #${replacement}" >/dev/null
		gh issue close "$number" --reason "not planned" >/dev/null
		info "closed #${number} tracking ${tracked}, superseded by #${replacement}"
	done < <(open_rows "$issues")
}

# close_caught_up closes every open issue for a component whose pin now matches
# upstream. All of them are stale, whatever version each one tracked.
# Args: issue array JSON, current version.
# Returns 0; a gh failure trips the ERR trap.
close_caught_up() {
	local issues="$1" version="$2"
	local number title
	while IFS=$'\t' read -r number title; do
		[[ -n "$number" ]] || continue
		gh issue comment "$number" --body "Pinned at ${version}" >/dev/null
		gh issue close "$number" --reason completed >/dev/null
		info "closed #${number} (${title}); the pin caught up"
	done < <(open_rows "$issues")
}

# ─── Per-component reconciliation ───────────────────────────────────────────

# handle_component brings one component's issues in line with the report. An
# exact title match in any state is left alone: a closed one is a decision, and
# reopening it would argue with whoever made it.
# Args: component key, version, outdated flag ("true"/"false"), issue body.
# Returns 0; a gh failure trips the ERR trap.
handle_component() {
	local component="$1" version="$2" outdated="$3" body="$4"
	local prefix="${TITLE_STEM} ${component} "
	local issues current
	# set -e does not reach inside a command substitution, so every helper that
	# returns a value through one is checked here by hand. Without this, a gh
	# outage reads as an empty issue list and the run files duplicates.
	issues="$(list_component_issues "$prefix")" ||
		die "could not list issues for ${component}"

	if [[ "$outdated" != "true" ]]; then
		close_caught_up "$issues" "$version"
		info "${component} is pinned at ${version}, nothing to file"
		return 0
	fi

	current="$(issue_number_for_title "$issues" "${prefix}${version}")" ||
		die "could not search the issue list for ${component} ${version}"
	if [[ -n "$current" ]]; then
		info "${component} ${version} is already tracked by #${current}"
	else
		current="$(create_issue "${prefix}${version}" "$body")" ||
			die "could not open an issue for ${component} ${version}"
		info "opened #${current} for ${component} ${version}"
	fi
	supersede_older "$issues" "$prefix" "$version" "$current"
}

# handle_runtime reconciles the llama.cpp runtime entry from the report.
# Args: report path.
# Returns 0; a gh or jq failure trips the ERR trap.
handle_runtime() {
	local input="$1"
	local name pinned pinned_build latest latest_build outdated url body
	IFS=$'\t' read -r name pinned pinned_build latest latest_build outdated url < <(
		jq -r '.runtime | [.name, .pinned, .pinned_build, .latest, .latest_build,
			(.outdated | tostring), .url] | @tsv' -- "$input"
	)
	body="$(printf '%s runtime pin is behind upstream.\n\npinned %s (build %s)\nlatest %s (build %s)\n%s\n\n%s\n' \
		"$name" "$pinned" "$pinned_build" "$latest" "$latest_build" "$url" "$FIX_LINE")"
	handle_component "$name" "$latest" "$outdated" "$body"
}

# handle_model reconciles one model entry. The title is keyed on the short
# weight hash, not the revision, because the hash decides `outdated`: a
# README-only commit moves the revision while the weights stand still.
# Args: name, repo, file, pinned revision, latest revision, pinned sha256,
#       latest sha256, outdated flag, url.
# Returns 0; a gh failure trips the ERR trap.
handle_model() {
	local name="$1" repo="$2" file="$3" pinned="$4" latest="$5"
	local pinned_sha="$6" latest_sha="$7" outdated="$8" url="$9"
	local body version="${latest_sha:0:12}"
	body="$(printf '%s (%s, %s) pin is behind upstream.\n\npinned revision %s\n  sha256 %s\nlatest revision %s\n  sha256 %s\n%s\n\n%s\n' \
		"$name" "$repo" "$file" "$pinned" "$pinned_sha" "$latest" "$latest_sha" "$url" "$FIX_LINE")"
	handle_component "$name" "$version" "$outdated" "$body"
}

# handle_models walks every model in the report.
# Args: report path.
# Returns 0; a gh failure trips the ERR trap.
handle_models() {
	local input="$1"
	local name repo file pinned latest pinned_sha latest_sha outdated url
	while IFS=$'\t' read -r name repo file pinned latest pinned_sha latest_sha outdated url; do
		[[ -n "$name" ]] || continue
		handle_model "$name" "$repo" "$file" "$pinned" "$latest" \
			"$pinned_sha" "$latest_sha" "$outdated" "$url"
	done < <(
		jq -r '.models[] | [.name, .repo, .file, .pinned_revision, .latest_revision,
			.pinned_sha256, .latest_sha256, (.outdated | tostring), .url] | @tsv' -- "$input"
	)
}

# main wires the run together.
# Returns 0 when every component reconciled; exits non-zero otherwise.
main() {
	local input="${1:-}"
	require_tools
	validate_input "$input"
	ensure_label
	handle_runtime "$input"
	handle_models "$input"
	info "model update check reconciled"
}

main "$@"
