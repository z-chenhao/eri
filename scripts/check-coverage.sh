#!/bin/sh

set -eu

minimum=${COVERAGE_MINIMUM:-60.0}
profile=$(mktemp "${TMPDIR:-/tmp}/eri-coverage.XXXXXX")
test_log=$(mktemp "${TMPDIR:-/tmp}/eri-coverage-test.XXXXXX")

cleanup() {
    rm -f "$profile" "$test_log"
}
trap cleanup EXIT HUP INT TERM

if ! go test -coverpkg=./... -coverprofile="$profile" ./... >"$test_log"; then
    cat "$test_log"
    exit 1
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ {gsub("%", "", $3); print $3}')
if [ -z "$total" ]; then
    echo "coverage-check: could not read total coverage" >&2
    exit 1
fi

if ! awk -v total="$total" -v minimum="$minimum" 'BEGIN { exit !(total + 0 >= minimum + 0) }'; then
    echo "coverage-check: total $total% is below required $minimum%" >&2
    exit 1
fi

printf 'coverage-check: cross-package statement coverage %s%% (minimum %s%%)\n' "$total" "$minimum"
