#!/bin/sh
# Install the exact Go toolchain from the lock, verifying its SHA-256.
# Usage: install-go.sh <dest-dir>   (prints the GOROOT/bin path to add to PATH)
. "$(dirname "$0")/lib.sh"
dest=${1:?dest dir}
url=$(lock go linux_amd64 url)
want=$(lock go linux_amd64 sha256)
mkdir -p "$dest"
tmp=$(mktemp -d)
curl -fsSL --retry 3 -o "$tmp/go.tgz" "$url"
got=$(sha256sum "$tmp/go.tgz" | cut -d' ' -f1)
[ "$got" = "$want" ] || die "Go archive hash mismatch: got $got, locked $want"
tar -C "$dest" -xzf "$tmp/go.tgz"
rm -rf "$tmp"
echo "$dest/go/bin"
