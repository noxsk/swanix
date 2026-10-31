#!/bin/sh
# Prepare the cronet toolchain for Full's with_naive_outbound (CGO + musl) and
# print `KEY=VALUE` environment lines to append to $GITHUB_ENV.
# Usage: cronet.sh <goarch> <workdir>
# Mirrors the upstream release workflow at the locked tag. Failure here stops
# the Full release for this target; it must never degrade to a smaller Full.
. "$(dirname "$0")/lib.sh"
arch=${1:?goarch}; work=${2:?workdir}
# stdout carries only the KEY=VALUE environment; everything else goes to stderr
exec 3>&1 1>&2
version=$(cat "$ROOT/.github/CRONET_GO_VERSION")
repo=$(lock full cronet repository)
rm -rf "$work/cronet-go"
git init -q "$work/cronet-go"
git -C "$work/cronet-go" remote add origin "$repo"
git -C "$work/cronet-go" sparse-checkout set --no-cone '/*' '!/lib'
git -C "$work/cronet-go" fetch -q --depth=1 --filter=blob:none origin "$version"
git -C "$work/cronet-go" checkout -q FETCH_HEAD
git -C "$work/cronet-go" submodule update --init --recursive --depth=1
rm -f "$work/cronet-go/naiveproxy/src/build/linux/sysroot_scripts/keyring.gpg"
(cd "$work/cronet-go" && GPG_TTY=/dev/null ./naiveproxy/src/build/linux/sysroot_scripts/generate_keyring.sh)
(cd "$work/cronet-go" && go run ./cmd/build-naive --target="linux/$arch" --libc=musl download-toolchain)
(cd "$work/cronet-go" && go run ./cmd/build-naive --target="linux/$arch" --libc=musl env) >&3
