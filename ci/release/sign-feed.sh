#!/bin/sh
# Index and sign the package feed with the SDK's own tooling.
# Usage: sign-feed.sh <series> <target> <package-dir> <out-dir>
# Secrets (protected `release` environment only):
#   ipk series: IPK_SIGN_KEY (usign secret key), IPK_SIGN_PUB
#   apk series: APK_SIGN_KEY (private-key.pem),  APK_SIGN_PUB (public-key.pem)
# The two key systems are independent (usign vs. apk's RSA/EC keys); never reuse one for the other.
. "$(dirname "$0")/../build/lib.sh"
series=${1:?series}; target=${2:?target}; pkgdir=${3:?package dir}; out=${4:?out dir}
fmt=$(lock openwrt "$series" package_format)
arch=$(lock openwrt "$series" targets "$target" package_arch)
work=$(mktemp -d)
sdk=$("$ROOT/ci/build/sdk.sh" "$series" "$target" "$work" | tail -1)
cd "$sdk"
dest=bin/packages/$arch/swanix
mkdir -p "$dest"
cp "$pkgdir"/*."$fmt" "$dest/"
umask 077
if [ "$fmt" = ipk ]; then
	[ -n "${IPK_SIGN_KEY:-}" ] && [ -n "${IPK_SIGN_PUB:-}" ] || die "IPK signing keys missing"
	printf '%s\n' "$IPK_SIGN_KEY" > key-build
	printf '%s\n' "$IPK_SIGN_PUB" > key-build.pub
else
	[ -n "${APK_SIGN_KEY:-}" ] && [ -n "${APK_SIGN_PUB:-}" ] || die "APK signing keys missing"
	printf '%s\n' "$APK_SIGN_KEY" > private-key.pem
	printf '%s\n' "$APK_SIGN_PUB" > public-key.pem
fi
echo "CONFIG_SIGNED_PACKAGES=y" >> .config
make defconfig >/dev/null
make package/index V=s
subtarget=${target#*/}
mkdir -p "$out/$series/${target%%/*}/$subtarget/$arch"
cp -r "$dest/." "$out/$series/${target%%/*}/$subtarget/$arch/"
# the keys never leave the SDK directory, and the SDK directory is deleted
rm -f key-build key-build.pub private-key.pem public-key.pem
case "$fmt" in
ipk) [ -f "$out/$series/${target%%/*}/$subtarget/$arch/Packages.sig" ] || die "feed was not signed (no Packages.sig)" ;;
apk) [ -f "$out/$series/${target%%/*}/$subtarget/$arch/packages.adb" ] || die "feed index missing (packages.adb)" ;;
esac
echo "signed feed written under $out/$series/"
