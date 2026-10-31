#!/bin/sh
# Unpack the locked OpenWrt SDK for one series/target, verify its hash, pin the
# package feeds to the locked commits and install our feed.
# Usage: sdk.sh <series> <target> <dest>   e.g. sdk.sh 24.10 x86/64 /tmp/sdk
. "$(dirname "$0")/lib.sh"
series=${1:?series}; target=${2:?target}; dest=${3:?dest}
url=$(lock openwrt "$series" targets "$target" sdk_url)
want=$(lock openwrt "$series" targets "$target" sdk_sha256)
name=$(lock openwrt "$series" targets "$target" sdk)
mkdir -p "$dest"
curl -fsSL --retry 3 -o "$dest/$name" "$url"
got=$(sha256sum "$dest/$name" | cut -d' ' -f1)
[ "$got" = "$want" ] || die "SDK hash mismatch for $name: got $got, locked $want"
mkdir -p "$dest/sdk"
tar -C "$dest/sdk" --strip-components=1 --zstd -xf "$dest/$name"
cd "$dest/sdk"
# feeds: official packages/luci pinned to locked commits, plus this repository's openwrt/ directory
pk=$(lock openwrt "$series" feeds packages commit)
lc=$(lock openwrt "$series" feeds luci commit)
pu=$(lock openwrt "$series" feeds packages url)
lu=$(lock openwrt "$series" feeds luci url)
cat > feeds.conf <<FEEDS
src-git-full packages $pu^$pk
src-git-full luci $lu^$lc
src-link swanix $ROOT/openwrt
FEEDS
./scripts/feeds update -a
./scripts/feeds install -a -p swanix
echo "$dest/sdk"
