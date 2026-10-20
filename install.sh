#!/bin/sh
# Installs the latest open-supervisor release:
#   curl -fsSL https://raw.githubusercontent.com/alsiola/open-supervisor/main/install.sh | sh
# Set OPEN_SUPERVISOR_VERSION=v0.1.0 for a particular one, and BIN_DIR to install somewhere other than
# ~/.local/bin.
set -eu

repo=alsiola/open-supervisor
bin_dir=${BIN_DIR:-"$HOME/.local/bin"}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in darwin|linux) ;; *) echo "unsupported OS: $os (on Windows, download the zip from the releases page)" >&2; exit 1 ;; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac

version=${OPEN_SUPERVISOR_VERSION:-}
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
fi
[ -n "$version" ] || { echo "couldn't find the latest release" >&2; exit 1; }
number=${version#v}
archive="open-supervisor_${number}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading open-supervisor $version for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1); else actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ "$expected" = "$actual" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp"
mkdir -p "$bin_dir"
install -m 0755 "$tmp/open-supervisor" "$bin_dir/open-supervisor"
echo "installed $bin_dir/open-supervisor"
case ":$PATH:" in *":$bin_dir:"*) ;; *) echo "add $bin_dir to your PATH" ;; esac
echo "next: open-supervisor init"
