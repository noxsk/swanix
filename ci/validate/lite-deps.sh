#!/bin/sh
# Fail if the Lite entry links anything outside the MultiWAN packages, the Go
# standard library and golang.org/x/sys. This inspects the real import graph
# (go list -deps); searching binary strings would prove nothing.
set -eu
cd "$(dirname "$0")/../.."
GOFLAGS=${GOFLAGS:-}
bad=$(go list -deps -tags mw_lite -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/swanix |
	grep -v -e '^github.com/sagernet/sing-box/experimental/multiwan/' \
	        -e '^github.com/sagernet/sing-box/cmd/swanix$' \
	        -e '^golang.org/x/sys/' || true)
if [ -n "$bad" ]; then
	echo "Lite build links forbidden packages:" >&2
	echo "$bad" >&2
	exit 1
fi
# the shared core must never depend on the proxy framework either
core=$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./experimental/multiwan/... |
	grep -v -e '^github.com/sagernet/sing-box/experimental/multiwan/' -e '^golang.org/x/sys/' || true)
if [ -n "$core" ]; then
	echo "experimental/multiwan depends on proxy framework packages:" >&2
	echo "$core" >&2
	exit 1
fi
echo "lite-deps: ok"
