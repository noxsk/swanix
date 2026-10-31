#!/usr/bin/env python3
"""Check a `swanix features --json` report against the reviewed expectations.

Usage: check-features.py <features.json> <lite|full>

Lite must report no proxy capability. Full must report every required build tag
and proxy feature of ci/locks/features.expected.json, except those listed for
its platform in platform_exceptions (each exception is a reviewed decision, not
a silent omission).
"""
import json
import pathlib
import sys

root = pathlib.Path(__file__).resolve().parents[2]
report = json.load(open(sys.argv[1]))
variant = sys.argv[2]
exp = json.load(open(root / "ci/locks/features.expected.json"))
errors = []

if report["profile"] != variant:
    errors.append(f"profile is {report['profile']}, expected {variant}")

if variant == "lite":
    if report["proxy_features"]:
        errors.append(f"Lite reports proxy features: {report['proxy_features'][:5]}...")
    if "mw_lite" not in report["build_tags"]:
        errors.append("Lite binary lacks the mw_lite build tag")
    for t in report["build_tags"]:
        if t.startswith("with_"):
            errors.append(f"Lite binary built with proxy tag {t}")
else:
    plat = f"{report['os']}/{report['arch']}"
    skipped = exp.get("platform_exceptions", {}).get(plat, {})
    for t in exp["required_build_tags"]:
        if t not in report["build_tags"] and t not in skipped.get("tags", []):
            errors.append(f"Full is missing required build tag {t} on {plat}")
    for f in exp["required_proxy_features"]:
        if f not in report["proxy_features"] and f not in skipped.get("features", []):
            errors.append(f"Full is missing required feature {f} on {plat}")
    if "mw_lite" in report["build_tags"]:
        errors.append("Full binary carries the mw_lite tag")
    if report["cgo"] != "1":
        errors.append("Full must be built with CGO (cronet); got cgo=" + report["cgo"])

if errors:
    print("features check FAILED:", file=sys.stderr)
    for e in errors:
        print(" -", e, file=sys.stderr)
    sys.exit(1)
print(f"features check ok ({variant})")
