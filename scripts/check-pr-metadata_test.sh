#!/bin/sh

set -eu

checker=${1:-./scripts/check-pr-metadata.sh}

expect_pass() {
    PR_HEAD_REF=$1 PR_TITLE=$2 PR_ACTOR=$3 "$checker" >/dev/null
}

expect_fail() {
    if PR_HEAD_REF=$1 PR_TITLE=$2 PR_ACTOR=$3 "$checker" >/dev/null 2>&1; then
        echo "expected metadata check to fail: branch=$1 title=$2 actor=$3" >&2
        exit 1
    fi
}

expect_pass 'feat/tool-add-file-search' 'feat(tool): add workspace file search' 'developer'
expect_pass 'fix/123-recover-outbox' 'fix(store): recover pending outbox delivery' 'codex'
expect_pass 'dependabot/go_modules/modernc.org/sqlite-1.55.0' 'build(deps): bump modernc.org/sqlite to 1.55.0' 'dependabot[bot]'

expect_fail 'feature/NewTool' 'feat(tool): add workspace file search' 'developer'
expect_fail 'feat/add-stuff' 'updated files' 'codex'
expect_fail 'fix/outbox' 'fix(store): recover pending delivery.' 'developer'

echo "pr-metadata-test: valid and invalid cases behave as expected"
