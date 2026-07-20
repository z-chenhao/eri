#!/bin/sh

set -eu

workflow_dir=.github/workflows

if [ ! -d "$workflow_dir" ]; then
    echo "workflow-check: $workflow_dir is missing" >&2
    exit 1
fi

find "$workflow_dir" -type f \( -name '*.yml' -o -name '*.yaml' \) -print | sort | while IFS= read -r workflow; do
    if [ ! -s "$workflow" ]; then
        echo "workflow-check: $workflow is empty" >&2
        exit 1
    fi

    if ! grep -Eq '^[[:space:]]*permissions:' "$workflow"; then
        echo "workflow-check: $workflow must declare permissions" >&2
        exit 1
    fi

    if ! grep -Eq '^[[:space:]]*timeout-minutes:' "$workflow"; then
        echo "workflow-check: $workflow must bound job runtime" >&2
        exit 1
    fi

    if grep -Eq '^[[:space:]]*(pull_request_target|workflow_run):' "$workflow"; then
        echo "workflow-check: $workflow uses a privileged trigger forbidden for untrusted code" >&2
        exit 1
    fi

	if grep -Eq 'runs-on:.*self-hosted|secrets\.[A-Za-z0-9_]+|^[[:space:]]*(contents|actions|attestations|checks|deployments|id-token|issues|packages|pull-requests|statuses):[[:space:]]*write' "$workflow"; then
		echo "workflow-check: $workflow introduces a forbidden runner, secret, or write permission" >&2
		exit 1
	fi

	if grep -Eq 'gh[[:space:]]+release[[:space:]]+create|actions/attest@' "$workflow"; then
		echo "workflow-check: unsigned 0.x candidates must not be published or attested as releases" >&2
		exit 1
	fi

    if grep -Eq 'uses:[[:space:]]*actions/checkout@' "$workflow" &&
        ! grep -Eq '^[[:space:]]*persist-credentials:[[:space:]]*false([[:space:]]|$)' "$workflow"; then
        echo "workflow-check: $workflow must disable persisted checkout credentials" >&2
        exit 1
    fi

    sed -n \
        -e 's/^[[:space:]]*-[[:space:]]*uses:[[:space:]]*//p' \
        -e 's/^[[:space:]]*uses:[[:space:]]*//p' \
        "$workflow" | while IFS= read -r use_line; do
        set -- $use_line
        action_ref=${1:-}
        case "$action_ref" in
            ./*) continue ;;
        esac
        if ! printf '%s\n' "$action_ref" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$'; then
            echo "workflow-check: $workflow contains an unpinned action: $action_ref" >&2
            exit 1
        fi
    done
done

echo "workflow-check: workflows use bounded jobs, minimal triggers, and pinned actions"
