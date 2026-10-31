#!/usr/bin/env python3
"""Refuse to release unless every cell of the locked matrix is present.

Usage: verify-matrix.py <artifact-dir>

Expects <dir>/<series>/<target>/<package-arch>/*.{ipk,apk} and <dir>/meta/.
Lite and Full are both mandatory for every target: there is no partial release.
"""
import json
import pathlib
import sys

root = pathlib.Path(__file__).resolve().parents[2]
lock = json.load(open(root / "ci/locks/lock.json"))
base = pathlib.Path(sys.argv[1])
errors = []

PACKAGES = ["swanix-common", "swanix-lite", "swanix-full", "luci-app-swanix", "luci-i18n-swanix-zh-cn"]

for series, s in lock["openwrt"].items():
    ext = s["package_format"]
    for target, t in s["targets"].items():
        d = base / series / target / t["package_arch"]
        if not d.is_dir():
            errors.append(f"missing {d.relative_to(base)}")
            continue
        names = [p.name for p in d.iterdir()]
        for pkg in PACKAGES:
            hits = [n for n in names if n.startswith(pkg + "_") or n.startswith(pkg + "-") and n[len(pkg) + 1:len(pkg) + 2].isdigit()]
            if not any(h.endswith("." + ext) for h in hits):
                errors.append(f"{series}/{target}: no native .{ext} for {pkg}")
        other = "apk" if ext == "ipk" else "ipk"
        if any(n.endswith("." + other) for n in names):
            errors.append(f"{series}/{target}: contains .{other} files in a {ext} series")

meta = base / "meta"
for variant, series in [(v, s) for v in ("lite", "full") for s in lock["openwrt"]]:
    f = meta / f"features-{variant}-amd64.json"
    if not f.exists() and not list(meta.glob(f"features-{variant}-*.json")):
        errors.append(f"no features report for {variant}")

if errors:
    print("matrix incomplete:", file=sys.stderr)
    for e in errors:
        print(" -", e, file=sys.stderr)
    sys.exit(1)
print("matrix complete")
