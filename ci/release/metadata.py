#!/usr/bin/env python3
"""Write the release metadata: manifest, features, sizes, test report, SBOM, checksums.

Usage: metadata.py <artifact-dir> <out-dir>
Environment: GITHUB_SHA, GITHUB_REF_NAME, TEST_RESULTS (JSON object job->result)
"""
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[2]
lock_path = root / "ci/locks/lock.json"
lock = json.load(open(lock_path))
base = pathlib.Path(sys.argv[1])
out = pathlib.Path(sys.argv[2])
out.mkdir(parents=True, exist_ok=True)


def sha256(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


packages = []
for series, s in lock["openwrt"].items():
    for target, t in s["targets"].items():
        d = base / series / target / t["package_arch"]
        for p in sorted(d.glob("*." + s["package_format"])):
            packages.append({
                "series": series, "target": target, "package_arch": t["package_arch"],
                "format": s["package_format"], "file": str(p.relative_to(base)),
                "bytes": p.stat().st_size, "sha256": sha256(p),
            })

manifest = {
    "project": "swanix",
    "version": os.environ.get("GITHUB_REF_NAME", "dev").lstrip("v"),
    "fork_commit": os.environ.get("GITHUB_SHA", "unknown"),
    "upstream": lock["upstream"],
    "go": lock["go"]["version"],
    "lock_sha256": sha256(lock_path),
    "openwrt": {k: {"release": v["release"], "format": v["package_format"], "feeds": v["feeds"]} for k, v in lock["openwrt"].items()},
    "packages": packages,
}
json.dump(manifest, open(out / "build-manifest.json", "w"), indent=2)

# features: the reports taken from the real binaries
features = {}
for f in sorted((base / "meta").glob("features-*.json")):
    features[f.stem.removeprefix("features-")] = json.load(open(f))
json.dump(features, open(out / "features.json", "w"), indent=2)

# sizes: package files and the uncompressed binaries
sizes = {"packages": [{k: p[k] for k in ("file", "bytes")} for p in packages], "binaries": []}
for b in sorted((base / "meta").glob("swanix-*-*-*")):
    if b.suffix == ".json":
        continue
    sizes["binaries"].append({"file": b.name, "bytes": b.stat().st_size})
sizes["note"] = "Measured sizes only. Memory and CPU are reported separately from runtime measurements; none are promised here."
json.dump(sizes, open(out / "size-report.json", "w"), indent=2)

# test report: what actually ran, and what did not
results = json.loads(os.environ.get("TEST_RESULTS", "{}"))
report = {
    "jobs": results,
    "covered": [
        "unit tests with the race detector (config, health, scheduler, kernel rendering, engine, session, doctor)",
        "kernel data path in network namespaces: per-connection balancing, weight update without breaking connections, reject on empty pool, router traffic, probe pinning, inbound port-forward return path, ownership-scoped removal",
        "Lite daemon end to end (probe, failover, recovery) and Full runtime with wan_policy, in namespaces",
        "native package build with the locked OpenWrt SDKs and install/switch/upgrade/remove in the series' x86-64 rootfs",
    ],
    "not_covered": [
        "OpenWrt x86 VM integration (real fw4, netifd, procd, hotplug)",
        "mediatek/filogic (arm64): built, not executed on hardware or a VM",
        "IPv6 native and NAT66 traffic end to end (rules are rendered and unit-tested only)",
        "flow offloading (software and hardware): stays off, no compatibility acceptance done",
        "DHCP renewal, PPPoE redial and prefix-delegation updates on real uplinks",
        "resource comparison (RSS/CPU) between Lite and Full on target hardware",
    ],
}
json.dump(report, open(out / "test-report.json", "w"), indent=2)

# SBOM (CycloneDX) from the Go build info of the amd64 binaries
components = {}
for b in sorted((base / "meta").glob("swanix-*-*-amd64")):
    txt = subprocess.run(["go", "version", "-m", str(b)], capture_output=True, text=True).stdout
    for line in txt.splitlines():
        m = re.match(r"\s+dep\s+(\S+)\s+(\S+)(?:\s+(\S+))?", line)
        if m:
            components[(m.group(1), m.group(2))] = m.group(3)
sbom = {
    "bomFormat": "CycloneDX", "specVersion": "1.5", "version": 1,
    "metadata": {"component": {"type": "application", "name": "swanix", "version": manifest["version"]}},
    "components": [{"type": "library", "name": n, "version": v, "purl": f"pkg:golang/{n}@{v}",
                    **({"hashes": [{"alg": "SHA-256", "content": h}]} if h and h.startswith("h1:") is False else {})}
                   for (n, v), h in sorted(components.items())],
}
json.dump(sbom, open(out / "sbom.cdx.json", "w"), indent=2)
print(f"wrote metadata for {len(packages)} packages, {len(components)} Go modules")
