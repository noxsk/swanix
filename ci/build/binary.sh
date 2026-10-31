#!/bin/sh
# Build one swanix binary with the locked toolchain.
# Usage: binary.sh <lite|full> <goarch> <outfile>
# Lite: pure Go, tag mw_lite. Full: the reviewed upstream tag set plus musl; the
# cronet toolchain environment must already be exported (ci/build/cronet.sh).
. "$(dirname "$0")/lib.sh"
variant=${1:?variant}; goarch=${2:?goarch}; out=${3:?out}
want_go=go$(lock go version)
have_go=$(go env GOVERSION)
[ "$have_go" = "$want_go" ] || die "toolchain is $have_go, lock requires $want_go"
export GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOOS=linux GOARCH=$goarch
BI=github.com/sagernet/sing-box/experimental/multiwan/buildinfo
VERSION=$(swanix_version)
COMMIT=${SWANIX_COMMIT:-$(git -C "$ROOT" rev-parse HEAD)}
X="-X $BI.Version=$VERSION -X $BI.Commit=$COMMIT -X $BI.OpenWrt=${SWANIX_OPENWRT:-} -X $BI.Verified=${SWANIX_VERIFIED:-}"
cd "$ROOT"
case "$variant" in
lite)
	CGO_ENABLED=0 go build -trimpath -tags mw_lite -ldflags "$X -s -w -buildid=" -o "$out" ./cmd/swanix
	;;
full)
	tags=$(tr -s ' \n' ',' < ci/locks/full-tags.txt | sed 's/,$//'),with_musl
	# the cronet environment supplies CC/CXX/CGO_LDFLAGS; CGO itself is switched on here
	case "${CC:-}" in *clang*) ;; *) die "Full needs the cronet toolchain environment (CC/CXX/CGO_LDFLAGS from ci/build/cronet.sh)" ;; esac
	export CGO_ENABLED=1
	go build -trimpath -tags "$tags" -ldflags "$X $(cat release/LDFLAGS) -s -w -buildid=" -o "$out" ./cmd/swanix
	;;
*) die "unknown variant $variant" ;;
esac
echo "built $out ($variant, $goarch, $VERSION)"
