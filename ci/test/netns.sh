#!/bin/sh
# Run kernel-touching tests inside a disposable network namespace so they can
# never modify the host's nftables, routes or rules. /run/netns is shadowed by
# a tmpfs so helper namespaces vanish with the private mount namespace.
set -eu
cd "$(dirname "$0")/../.."
# The namespace has no network: fetch and compile everything first, outside it.
go test -count=1 -run '^$' "$@" >/dev/null
exec unshare --net --mount --propagation private sh -c '
	mkdir -p /run/netns
	mount -t tmpfs none /run/netns
	mkdir -p /run/swanix
	mount -t tmpfs none /run/swanix
	mount -t sysfs none /sys  # interface counters of this namespace, not those of the host
	ip link set lo up
	SWANIX_NETNS=1 go test -count=1 "$@"
' sh "$@"
