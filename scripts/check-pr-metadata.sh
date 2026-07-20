#!/bin/sh

set -eu

: "${PR_HEAD_REF:?PR_HEAD_REF is required}"
: "${PR_TITLE:?PR_TITLE is required}"
: "${PR_ACTOR:?PR_ACTOR is required}"

fail() {
    echo "pr-metadata: $*" >&2
    exit 1
}

branch_pattern='^(feat|fix|refactor|docs|test|chore|build|ci|perf|hotfix)/[a-z0-9]+(-[a-z0-9]+)*$'
title_pattern='^(feat|fix|refactor|docs|test|chore|build|ci|perf|revert)(\([a-z0-9]+(-[a-z0-9]+)*\))?!?: [a-z0-9][[:print:]]*$'

if [ "$PR_ACTOR" = 'dependabot[bot]' ]; then
    case "$PR_HEAD_REF" in
        dependabot/*) ;;
        *) fail "Dependabot branch must start with dependabot/" ;;
    esac
elif ! printf '%s\n' "$PR_HEAD_REF" | grep -Eq "$branch_pattern"; then
    fail "branch '$PR_HEAD_REF' must match <type>/<kebab-case-description>"
fi

first_title_line=$(printf '%s\n' "$PR_TITLE" | sed -n '1p')
if [ "$first_title_line" != "$PR_TITLE" ]; then
    fail "PR title must be a single line"
fi

if [ "${#PR_TITLE}" -gt 72 ]; then
    fail "PR title must not exceed 72 characters"
fi

if ! printf '%s\n' "$PR_TITLE" | grep -Eq "$title_pattern"; then
    fail "PR title must be a lowercase Conventional Commit header"
fi

case "$PR_TITLE" in
    *.) fail "PR title must not end with a period" ;;
esac

echo "pr-metadata: branch and title are valid"
