#!/bin/sh
# herdr [[build]] step for `herdr plugin install`. Downloads the release archive matching this
# manifest's version for the host OS/arch (herdr-orch-v<ver>-<os>-<arch>.tar.gz), verifies it
# against the release's checksums.txt, and extracts herdr-orch and horch into ./bin. On any miss
# (no release, unsupported platform, no curl, checksum mismatch) it falls back to `go build`, so
# Go is only needed when there is no prebuilt match.
set -eu

repo=ellingtonsp/herdr-orch
version=$(sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml | head -1)

os=$(uname -s); arch=$(uname -m)
case "$os" in Darwin) os=macos ;; Linux) os=linux ;; *) os="" ;; esac
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) arch="" ;; esac

sha256() { shasum -a 256 "$1" 2>/dev/null | awk '{print $1}' || sha256sum "$1" | awk '{print $1}'; }

build_from_source() {
  if command -v go >/dev/null 2>&1; then
    echo "herdr-orch: building from source with $(go version | awk '{print $3}')"
    ldflags="-s -w -X github.com/ellingtonsp/herdr-orch/internal/daemon.Version=v${version:-dev}"
    mkdir -p bin
    go build -trimpath -ldflags "$ldflags" -o bin/herdr-orch ./cmd/herdr-orch
    go build -trimpath -ldflags "$ldflags" -o bin/horch ./cmd/horch
    exit 0
  fi
  echo "herdr-orch: no prebuilt release for ${os:-?}/${arch:-?} v${version:-?} and Go is not installed." >&2
  echo "  Install Go 1.26+ to build from source, or use macOS/Linux on amd64/arm64." >&2
  exit 1
}

[ -n "$os" ] && [ -n "$arch" ] && [ -n "$version" ] && command -v curl >/dev/null 2>&1 || build_from_source

base="${HERDR_ORCH_RELEASE_BASE:-https://github.com/${repo}/releases/download/v${version}}"
archive="herdr-orch-v${version}-${os}-${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if curl -fsSL "$base/$archive" -o "$tmp/$archive" && curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"; then
  want=$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt")
  if [ -n "$want" ] && [ "$want" = "$(sha256 "$tmp/$archive")" ]; then
    tar -xzf "$tmp/$archive" -C "$tmp"
    dir="$tmp/herdr-orch-v${version}-${os}-${arch}"
    mkdir -p bin
    mv "$dir/herdr-orch" "$dir/horch" bin/
    chmod +x bin/herdr-orch bin/horch
    echo "herdr-orch: installed prebuilt v$version for $os/$arch"
    exit 0
  fi
  echo "herdr-orch: checksum mismatch for $archive" >&2
fi
echo "herdr-orch: prebuilt release unavailable; falling back to source" >&2
build_from_source
