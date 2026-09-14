#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (C) 2026 SukramJ
#
# Extract the changelog.md section for the given version and emit it
# as a self-contained release-notes payload on stdout. Single source
# of truth shared by `make release-notes` (local dry-run) and the
# .github/workflows/release-on-tag.yml workflow.
#
# Uses only POSIX-compatible awk + sed so it runs the same on macOS
# (BSD awk) and Ubuntu (gawk) — no `match($0, regex, array)` tricks.
#
# Usage: script/extract-release-notes.sh <version>
#
# Exits non-zero when no matching section is found, so `make release`
# fails fast instead of producing an empty release body.

set -euo pipefail

if [ $# -lt 1 ]; then
	echo "usage: $0 <version>" >&2
	exit 2
fi

VERSION="$1"
CHANGELOG="${CHANGELOG:-changelog.md}"

if [ ! -f "$CHANGELOG" ]; then
	echo "error: $CHANGELOG not found at $(pwd)" >&2
	exit 1
fi

# Body: skip the header line itself, print everything until the next
# "# Version " header (or EOF).
body=$(awk -v ver="$VERSION" '
	/^# Version / {
		if (insec) exit
		if ($0 ~ "^# Version " ver " ") { insec=1; next }
	}
	insec { print }
' "$CHANGELOG")

if [ -z "$body" ]; then
	echo "error: no '# Version $VERSION ' section found in $CHANGELOG" >&2
	exit 1
fi

# Previous version: the next "# Version <tag> ..." header that appears
# after our section. Splitting the regex/extraction into awk+sed keeps
# us off the gawk-only match-with-array form.
prev_header=$(awk -v ver="$VERSION" '
	$0 ~ "^# Version " ver " " { insec=1; next }
	insec && /^# Version / { print; exit }
' "$CHANGELOG")

prev_version=""
if [ -n "$prev_header" ]; then
	prev_version=$(printf '%s\n' "$prev_header" | sed -E 's/^# Version ([^ ]+).*$/\1/')
fi

# Assemble the payload in memory first, so it can be measured before
# anything is emitted.
payload=$(printf '%s\n' "$body")

# Changelog headers carry a bare version ("# Version 0.12.0"); the tags
# they correspond to are v-prefixed ("v0.12.0"). Comparing the bare form
# 404s, which is what every release this repo has published so far
# shipped. Resolve each side to a ref that actually exists, preferring
# the v-prefixed tag — this repo's very first tag (0.1.0) is bare, so a
# blanket "v" prefix would merely move the 404 rather than remove it.
tag_ref() {
	if git rev-parse -q --verify "refs/tags/v$1" >/dev/null 2>&1; then
		printf 'v%s' "$1"
	elif git rev-parse -q --verify "refs/tags/$1" >/dev/null 2>&1; then
		printf '%s' "$1"
	else
		# Not fetched (shallow clone), or — the normal case for the
		# release being cut — the tag is created by the very push that
		# triggers the workflow. Assume the convention every tag in
		# this repo but the first one follows.
		printf 'v%s' "$1"
	fi
}

# Emit the body, then optionally the compare link. The first release
# has no predecessor — that's fine, just skip the link.
if [ -n "$prev_version" ]; then
	repo="${GITHUB_REPOSITORY:-SukramJ/go-daikin2mqtt}"
	from_ref=$(tag_ref "$prev_version")
	to_ref=$(tag_ref "$VERSION")
	link=$(printf '\n\n**Full Changelog**: https://github.com/%s/compare/%s...%s' \
		"$repo" "$from_ref" "$to_ref")
	payload="${payload}${link}"
fi

# GitHub rejects a release body over 125000 characters with HTTP 422 —
# and by then the tag is already on the remote, so the failure is not
# recoverable by editing the changelog alone. Truncate instead, leaving
# a pointer to the full section. The limit is characters, not bytes;
# counting bytes is the conservative side of that (a multi-byte body
# is truncated slightly early, never late).
MAX_BODY_BYTES="${MAX_BODY_BYTES:-125000}"
# Room for the notice appended below.
NOTICE_BUDGET=400

actual=$(printf '%s' "$payload" | wc -c | tr -d ' ')
if [ "$actual" -gt "$MAX_BODY_BYTES" ]; then
	keep=$((MAX_BODY_BYTES - NOTICE_BUDGET))
	repo="${GITHUB_REPOSITORY:-SukramJ/go-daikin2mqtt}"
	echo "warning: release notes for $VERSION are $actual bytes, over the" >&2
	echo "         GitHub limit of $MAX_BODY_BYTES — truncating to $keep bytes." >&2
	printf '%s' "$payload" | head -c "$keep"
	printf '\n\n---\n\n*These release notes were truncated at %s bytes to stay under\nGitHub'"'"'s %s-character release-body limit. The complete section for\n%s is in [changelog.md](https://github.com/%s/blob/%s/changelog.md).*\n' \
		"$keep" "$MAX_BODY_BYTES" "$VERSION" "$repo" "v$VERSION"
	exit 0
fi

printf '%s\n' "$payload"
