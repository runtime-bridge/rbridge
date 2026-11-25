#!/bin/sh
# Installs the latest rbridge release, or the version given as $1, into
# $RBRIDGE_INSTALL_DIR (default ~/.local/bin). The archive is checked against
# the release's checksums.txt before anything is written, and against its
# cosign signature when cosign is installed.
set -eu

repo="runtime-bridge/rbridge"
dir="${RBRIDGE_INSTALL_DIR:-$HOME/.local/bin}"
version="${1:-}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "rbridge install: $1 is required" >&2; exit 1; }; }
need curl
need tar
if command -v sha256sum >/dev/null 2>&1; then sha="sha256sum"; else need shasum; sha="shasum -a 256"; fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux|darwin) ;; *) echo "rbridge install: unsupported OS $os; download a release from https://github.com/$repo/releases" >&2; exit 1 ;; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "rbridge install: unsupported architecture $arch" >&2; exit 1 ;; esac

# Without a published release, Go can build the latest source instead.
from_source() {
  if command -v go >/dev/null 2>&1; then
    echo "no rbridge release is published yet; building the latest source with Go"
    mkdir -p "$dir"
    GOBIN="$dir" CGO_ENABLED=0 go install "github.com/$repo/cmd/rbridge@latest"
    echo "installed $dir/rbridge"
    case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to your PATH to run rbridge" ;; esac
    "$dir/rbridge" version
    exit 0
  fi
  echo "rbridge install: no release is published yet. Install Go 1.25 or later and run this script again," >&2
  echo "or build from source: git clone https://github.com/$repo && cd rbridge && make build" >&2
  exit 1
}

if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1) || true
  [ -n "$version" ] || from_source
fi
number=${version#v}
archive="rbridge_${number}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading rbridge $version for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

if command -v cosign >/dev/null 2>&1; then
  curl -fsSL -o "$tmp/checksums.txt.sig" "$base/checksums.txt.sig"
  curl -fsSL -o "$tmp/checksums.txt.pem" "$base/checksums.txt.pem"
  cosign verify-blob --certificate "$tmp/checksums.txt.pem" --signature "$tmp/checksums.txt.sig" \
    --certificate-identity-regexp "^https://github.com/$repo/" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com "$tmp/checksums.txt" >/dev/null
  echo "signature verified"
else
  echo "cosign not found; checking the checksum only"
fi

expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d ' ' -f 1)
[ -n "$expected" ] || { echo "rbridge install: $archive is not listed in checksums.txt" >&2; exit 1; }
actual=$(cd "$tmp" && $sha "$archive" | cut -d ' ' -f 1)
[ "$expected" = "$actual" ] || { echo "rbridge install: checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp" rbridge
mkdir -p "$dir"
install -m 0755 "$tmp/rbridge" "$dir/rbridge"
echo "installed $dir/rbridge"
case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to your PATH to run rbridge" ;; esac
"$dir/rbridge" version
