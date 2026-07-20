#!/bin/sh

set -eu

version=${1:-}
output_dir=${2:-dist}

if [ ! -f .go-version ]; then
    echo "release-build: .go-version is missing" >&2
    exit 1
fi

required_go_version=$(tr -d '[:space:]' <.go-version)
current_go_version=$(go env GOVERSION)

if [ "$current_go_version" != "go$required_go_version" ]; then
    echo "release-build: Go $required_go_version is required; found $current_go_version" >&2
    exit 1
fi

if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z][0-9A-Za-z.-]*)?$'; then
    echo "release-build: version must be SemVer with a v prefix" >&2
    exit 1
fi

if [ -d "$output_dir" ] && [ -n "$(find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
    echo "release-build: output directory is not empty: $output_dir" >&2
    exit 1
fi

mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/eri-release.XXXXXX")

cleanup() {
    rm -rf "$build_root"
}
trap cleanup EXIT HUP INT TERM

release_version=${version#v}

for arch in amd64 arm64; do
    package="eri_${release_version}_darwin_${arch}"
    package_dir="$build_root/$package"
    mkdir -p "$package_dir"

    GOOS=darwin GOARCH=$arch CGO_ENABLED=0 \
        go build -trimpath -ldflags='-s -w' -o "$package_dir/eri" ./cmd/eri
    GOOS=darwin GOARCH=$arch CGO_ENABLED=0 \
        go build -trimpath -ldflags='-s -w' -o "$package_dir/eri-google-workspace" ./cmd/eri-google-workspace
    GOOS=darwin GOARCH=$arch CGO_ENABLED=0 \
        go build -trimpath -ldflags='-s -w' -o "$package_dir/eri-google-auth-broker" ./cmd/eri-google-auth-broker
    chmod 0755 "$package_dir/eri" "$package_dir/eri-google-workspace" "$package_dir/eri-google-auth-broker"
    mkdir -p "$package_dir/plugins"
    cp plugins/google-workspace.json "$package_dir/plugins/"
    cp LICENSE NOTICE README.md SECURITY.md THIRD_PARTY_NOTICES.md "$package_dir/"
    go run ./scripts/notices -output "$package_dir/THIRD_PARTY_LICENSES.txt"
    test -s "$package_dir/THIRD_PARTY_LICENSES.txt"

    go run ./scripts/archive -source "$package_dir" -output "$output_dir/$package.tar.gz" -prefix "$package"
done

(
    cd "$output_dir"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum ./*.tar.gz >SHA256SUMS
    else
        shasum -a 256 ./*.tar.gz >SHA256SUMS
    fi
)

echo "release-build: created macOS amd64 and arm64 archives in $output_dir"
