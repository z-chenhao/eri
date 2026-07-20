#!/bin/sh

set -eu

required_files="
.go-version
AGENTS.md
README.md
CONTRIBUTING.md
LICENSE
NOTICE
SECURITY.md
CODE_OF_CONDUCT.md
THIRD_PARTY_NOTICES.md
go.mod
go.sum
cmd/eri/main.go
cmd/eri-google-workspace/main.go
cmd/eri-google-auth-broker/main.go
api/plugin/v1/auth.go
plugins/google-workspace.json
docs/development/ci-cd.md
docs/development/git-conventions.md
docs/mvp-product.md
docs/mvp-technical.md
.github/CODEOWNERS
.github/dependabot.yml
.github/pull_request_template.md
.github/ISSUE_TEMPLATE/bug_report.yml
.github/ISSUE_TEMPLATE/feature_request.yml
.github/ISSUE_TEMPLATE/config.yml
.github/workflows/quality.yml
.github/workflows/release.yml
.github/workflows/security.yml
scripts/build-release.sh
scripts/archive/main.go
scripts/archive/main_test.go
scripts/check-pr-metadata.sh
scripts/check-pr-metadata_test.sh
scripts/check-coverage.sh
scripts/architecture/architecture_test.go
scripts/repository/repository_test.go
scripts/check-workflows.sh
"

for file in $required_files; do
    if [ ! -s "$file" ]; then
        echo "required file is missing or empty: $file" >&2
        exit 1
    fi
done

if ! grep -Fq 'Apache License' LICENSE || ! grep -Fq 'Version 2.0, January 2004' LICENSE; then
    echo "LICENSE must contain the Apache License 2.0 text" >&2
    exit 1
fi

if ! grep -Fq 'Copyright 2026 Eri contributors' NOTICE; then
    echo "NOTICE must identify the Eri project attribution" >&2
    exit 1
fi

if ! grep -Fq '(./mvp-technical.md)' docs/mvp-product.md; then
    echo "product document must link to the technical source of truth" >&2
    exit 1
fi

if ! grep -Fq '(./mvp-product.md)' docs/mvp-technical.md; then
    echo "technical document must link to the product source of truth" >&2
    exit 1
fi

if ! grep -Fq 'This document is the single product source of truth for the Eri MVP.' docs/mvp-product.md; then
    echo "product source-of-truth marker is missing" >&2
    exit 1
fi

if ! grep -Fq 'This document is the single technical source of truth for the Eri MVP.' docs/mvp-technical.md; then
    echo "technical source-of-truth marker is missing" >&2
    exit 1
fi

if grep -R -q -E --exclude-dir=.git --exclude-dir=.eri --exclude-dir=bin --exclude-dir=node_modules --exclude='*.sum' 'sk-[A-Za-z0-9]{20,}' .; then
    echo "repository contains a value that looks like an API credential" >&2
    exit 1
fi

if ! grep -Fq '"redemption_endpoint_environment": "ERI_GOOGLE_AUTH_REDEMPTION_BROKER"' plugins/google-workspace.json; then
    echo "Google Workspace manifest must separate capability issuance from redemption" >&2
    exit 1
fi

if ! grep -Fq 'broker.IssuerHandler()' cmd/eri-google-auth-broker/main.go || ! grep -Fq 'broker.PluginHandler()' cmd/eri-google-auth-broker/main.go; then
    echo "Google Auth Broker must expose separate Core and plugin handlers" >&2
    exit 1
fi

echo "repo-check: authoritative documents and links are valid"
