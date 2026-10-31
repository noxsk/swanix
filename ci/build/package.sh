#!/bin/sh
# Build the OpenWrt packages with the SDK's native package system.
# Usage: package.sh <sdk-dir> <series> <prebuilt-dir> <lite|full>
# <prebuilt-dir> holds swanix-lite and swanix-full built by binary.sh; the SDK
# is the final packager (ipk on 24.10, apk on 25.12), so file names, dependency
# metadata and indexes are exactly what the series' package manager expects.
. "$(dirname "$0")/lib.sh"
sdk=${1:?sdk dir}; series=${2:?series}; pre=${3:?prebuilt dir}; variant=${4:?lite|full}
fmt=$(lock openwrt "$series" package_format)
cd "$sdk"
export SWANIX_VERSION=$(swanix_version) SWANIX_PREBUILT=$pre SWANIX_SRC=$ROOT SWANIX_OPENWRT=$series
# One variant per job. The shared packages (common, LuCI) are built by the Lite job only.
{
	echo "CONFIG_ALL_NONSHARED=n"; echo "CONFIG_ALL_KMODS=n"; echo "CONFIG_ALL=n"
	echo "CONFIG_AUTOREMOVE=n"; echo "CONFIG_SIGNED_PACKAGES=n"
	echo "CONFIG_PACKAGE_swanix-$variant=m"
	if [ "$variant" = lite ]; then
		echo "CONFIG_PACKAGE_swanix-common=m"; echo "CONFIG_PACKAGE_luci-app-swanix=m"; echo "CONFIG_LUCI_LANG_zh_Hans=y"
	fi
} > .config
make defconfig
# NO_DEPS: our packages only have runtime dependencies. Without it the SDK tries to
# build luci-base's compiled dependencies (libucode headers are not part of the SDK).
if [ "$variant" = lite ]; then
	make package/feeds/luci/luci-base/host/compile NO_DEPS=1 V=s   # po2lmo for the translations
	make package/feeds/swanix/swanix-common/compile package/feeds/swanix/luci-app-swanix/compile NO_DEPS=1 V=s
fi
make package/feeds/swanix/swanix/clean NO_DEPS=1 >/dev/null  # the prebuilt binary is the input: never reuse a stamp
make package/feeds/swanix/swanix/compile NO_DEPS=1 V=s
find bin/packages -type f \( -name '*.ipk' -o -name '*.apk' \) | sort
[ "$fmt" = "ipk" ] && ext=ipk || ext=apk
n=$(find bin/packages -name "swanix-$variant*.$ext" | wc -l)
[ "$n" -ge 1 ] || die "no native $ext produced"
