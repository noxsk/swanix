#!/bin/sh
# Shared helpers. Source this file; it reads ci/locks/lock.json.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LOCK="$ROOT/ci/locks/lock.json"

lock() { python3 - "$LOCK" "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for k in sys.argv[2:]:
    d = d[k]
print(d if not isinstance(d, (list, dict)) else json.dumps(d))
PY
}

die() { echo "error: $*" >&2; exit 1; }

# Version string valid for both package managers (opkg and apk): digits and dots, with an
# optional _alpha/_beta/_pre/_rc/_git/_p suffix. Tags such as v1.2.3-rc1 map to 1.2.3_rc1.
swanix_version() {
	if [ -n "${SWANIX_VERSION:-}" ]; then v=$SWANIX_VERSION
	else
		tag=$(git -C "$ROOT" describe --tags --exact-match 2>/dev/null || true)
		if [ -n "$tag" ]; then v=$(printf '%s' "${tag#v}" | sed 's/-\(alpha\|beta\|pre\|rc\)\([0-9]*\)$/_\1\2/')
		else v="0.0.0_git$(git -C "$ROOT" log -1 --format=%cd --date=format:%Y%m%d)"; fi
	fi
	printf '%s' "$v" | grep -Eq '^[0-9]+(\.[0-9]+)*(_(alpha|beta|pre|rc|git|p)[0-9]*)?$' ||
		die "version '$v' is not valid for apk/opkg (use e.g. 1.2.3 or 1.2.3_rc1)"
	echo "$v"
}
