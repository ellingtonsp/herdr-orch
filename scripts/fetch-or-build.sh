#!/bin/sh
# herdr [[build]] step for `herdr plugin install`. Downloads the prebuilt herdr-orch and horch
# binaries matching this manifest's version for the host OS/arch, verifies their SHA-256, and
# puts them in ./bin. On any miss (no release, unsupported platform, no curl, checksum
# mismatch) it falls back to `go build`, so Go is only needed when there is no prebuilt match.
set -eu

repo=ellingtonsp/herdr-orch
version=$(sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml | head -1)

os=$(uname -s); arch=$(uname -m)
case "$os" in Darwin) os=darwin ;; Linux) os=linux ;; *) os="" ;; esac
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
  echo "herdr-orch: no prebuilt binaries for ${os:-?}/${arch:-?} v${version:-?} and Go is not installed." >&2
  echo "  Install Go 1.26+ to build from source, or use macOS/Linux on amd64/arm64." >&2
  exit 1
}

[ -n "$os" ] && [ -n "$arch" ] && [ -n "$version" ] && command -v curl >/dev/null 2>&1 || build_from_source

base="${HERDR_ORCH_RELEASE_BASE:-https://github.com/${repo}/releases/download/v${version}}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch() { # fetch <binary>: download and verify into $tmp, or fail
  asset="$1-${os}-${arch}"
  curl -fsSL "$base/$asset" -o "$tmp/$1" && curl -fsSL "$base/$asset.sha256" -o "$tmp/$1.sha256" || return 1
  [ "$(cat "$tmp/$1.sha256")" = "$(sha256 "$tmp/$1")" ] || { echo "herdr-orch: checksum mismatch for $asset" >&2; return 1; }
  chmod +x "$tmp/$1"
}

if fetch herdr-orch && fetch horch; then
  mkdir -p bin
  mv "$tmp/herdr-orch" "$tmp/horch" bin/
  echo "herdr-orch: installed prebuilt v$version for $os/$arch"
  exit 0
fi
echo "herdr-orch: prebuilt download unavailable; falling back to source" >&2
build_from_source
