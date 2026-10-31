#!/bin/sh
# Install the built native packages inside the series' official x86-64 rootfs
# (chroot, run as root) and exercise the package manager: dependencies, mutual
# exclusion of the two cores, config preservation across a variant switch,
# upgrade and removal.
#
# This proves the packages install and the binary starts under that userland.
# It does NOT prove netfilter/netifd/procd integration; that needs an OpenWrt
# VM and is tracked separately in docs/STATUS.md.
#
# Usage: package-smoke.sh <series> <dir-with-packages>
. "$(dirname "$0")/../build/lib.sh"
series=${1:?series}; pkgdir=$(cd "${2:?pkg dir}" && pwd)
fmt=$(lock openwrt "$series" package_format)
url=$(lock openwrt "$series" targets x86/64 rootfs_url)
want=$(lock openwrt "$series" targets x86/64 rootfs_sha256)
[ "$(id -u)" = 0 ] || die "run as root (chroot, mount)"

work=$(mktemp -d)
cleanup() { umount "$work/root/proc" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
curl -fsSL --retry 3 -o "$work/rootfs.tgz" "$url"
[ "$(sha256sum "$work/rootfs.tgz" | cut -d' ' -f1)" = "$want" ] || die "rootfs hash mismatch"
mkdir -p "$work/root"
tar -C "$work/root" --numeric-owner -xzf "$work/rootfs.tgz"
mkdir -p "$work/root/pkgs" "$work/root/proc" "$work/root/tmp" "$work/root/var/lock"
cp "$pkgdir"/*."$fmt" "$work/root/pkgs/"
rm -f "$work/root/etc/resolv.conf"  # dangling symlink into /tmp in the stock image
cp /etc/resolv.conf "$work/root/etc/resolv.conf"
mount -t proc proc "$work/root/proc"
# The stock image has no CA bundle for its own downloader. Feed indexes are signed (opkg
# check_signature, apk keys), so fetching them over plain HTTP does not weaken the check.
for f in "$work"/root/etc/opkg/distfeeds.conf "$work"/root/etc/apk/repositories.d/*; do
	[ -f "$f" ] && sed -i 's|https://downloads.openwrt.org|http://downloads.openwrt.org|g' "$f"
done
inroot() { chroot "$work/root" /bin/sh -c "$1"; }

pick() { ls "$work/root/pkgs" | grep -E "^$1[-_][0-9]" | head -1; }
common=$(pick swanix-common); lite=$(pick swanix-lite); full=$(pick swanix-full); luci=$(pick luci-app-swanix)
[ -n "$common" ] && [ -n "$lite" ] && [ -n "$full" ] && [ -n "$luci" ] || die "missing a package in $pkgdir: $(ls "$work/root/pkgs")"

# SWANIX_SMOKE_OFFLINE=1 skips feed access and waives dependency resolution; it exists
# for developers behind a TLS-intercepting proxy and must not be used in CI.
if [ "$fmt" = ipk ]; then
	if [ "${SWANIX_SMOKE_OFFLINE:-}" = 1 ]; then
		fd="--force-depends"
	else
		inroot "opkg update >/dev/null"
		fd=""
	fi
	install() {
		inroot "opkg install $fd $(for p in "$@"; do printf '/pkgs/%s ' "$p"; done)" || [ "${SWANIX_SMOKE_OFFLINE:-}" = 1 ]
	}
	remove() { inroot "opkg remove $*"; }
	listfiles() { inroot "opkg files $1"; }
else
	if [ "${SWANIX_SMOKE_OFFLINE:-}" = 1 ]; then
		fd="--no-network --force-broken-world"
	else
		inroot "apk update >/dev/null"
		fd=""
	fi
	# the CI test build is signed with an ephemeral key, so trust must be waived here only
	install() {
		inroot "apk add --allow-untrusted $fd $(for p in "$@"; do printf '/pkgs/%s ' "$p"; done)" || [ "${SWANIX_SMOKE_OFFLINE:-}" = 1 ]
	}
	remove() { inroot "apk del $*"; }
	listfiles() { inroot "apk info -L $1"; }
fi

echo "== install common + lite"
install "$common" "$lite"
inroot "/usr/bin/swanix version" | grep -q 'lite' || die "swanix-lite does not report the lite profile"
inroot "/usr/bin/swanix check" | grep -q '^OK' || die "default config does not validate"
inroot "/usr/bin/swanix features --json" > "$work/features-lite.json"
python3 "$ROOT/ci/release/check-features.py" "$work/features-lite.json" lite
inroot "test -x /etc/init.d/swanix && test -x /usr/libexec/rpcd/swanix && test -f /etc/hotplug.d/iface/90-swanix" || die "files missing"
[ "$(inroot "uci -q get swanix.global.enabled")" = 0 ] || die "installation must leave swanix disabled"
[ "$(inroot "uci -q get swanix.global.startup_mode")" = observe ] || die "installation must be observe-only"

echo "== the two cores are mutually exclusive"
if [ "${SWANIX_SMOKE_OFFLINE:-}" = 1 ]; then
	echo "SKIPPED (offline: opkg reaches the conflict check only after dependencies resolve)"
else
	if install "$full" >"$work/conflict.log" 2>&1; then
		cat "$work/conflict.log"; die "swanix-full installed next to swanix-lite"
	fi
fi

echo "== user configuration survives removing and reinstalling the core"
inroot "uci set swanix.wan1.download_mbps=123 && uci commit swanix"
remove swanix-lite
inroot "test -f /etc/config/swanix" || die "config removed with the lite core"
if [ "${SWANIX_SMOKE_OFFLINE:-}" = 1 ]; then
	install "$lite"
	[ "$(inroot "uci -q get swanix.wan1.download_mbps")" = 123 ] || die "config not preserved across reinstall"
	echo "SKIPPED (offline): switching to swanix-full needs the kmod dependencies from the feeds"
else
	install "$full"
	[ "$(inroot "uci -q get swanix.wan1.download_mbps")" = 123 ] || die "config not preserved across the switch"
	inroot "/usr/bin/swanix version" | grep -q 'full' || die "swanix-full does not report the full profile"
	inroot "test -x /usr/libexec/swanix/full" || die "full marker missing"

	echo "== the conflict is enforced in the other direction too"
	if install "$lite" >"$work/conflict2.log" 2>&1; then
		cat "$work/conflict2.log"; die "swanix-lite installed next to swanix-full"
	fi
fi

echo "== LuCI package"
install "$luci"
inroot "test -f /usr/share/rpcd/acl.d/luci-app-swanix.json && test -f /www/luci-static/resources/view/swanix/overview.js" || die "LuCI files missing"
inroot "/usr/libexec/rpcd/swanix list" | python3 -c 'import json,sys; assert "apply" in json.load(sys.stdin)' || die "rpcd plugin list broken"

echo "== upgrade in place"
if [ "${SWANIX_SMOKE_OFFLINE:-}" = 1 ]; then core=$lite; corename=swanix-lite; else core=$full; corename=swanix-full; fi
install "$core"

echo "== removal withdraws only what we own"
remove luci-app-swanix "$corename" swanix-common
inroot "test ! -e /usr/bin/swanix && test ! -e /etc/init.d/swanix" || die "files left behind"
echo "package smoke ok ($series, $fmt)"
