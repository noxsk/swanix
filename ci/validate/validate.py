#!/usr/bin/env python3
"""Static checks run by the validate job. Exits non-zero on any violation.

* the lock file is complete and contains no floating references
* every `uses:` in the workflows is pinned to a full commit SHA that the lock knows
* Full's tag list equals the upstream release/DEFAULT_BUILD_TAGS (no silent trimming)
* the sample configuration validates with the Lite and Full profiles
* go.mod's go directive is not newer than the locked toolchain
"""
import json
import pathlib
import re
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[2]
errors = []


def err(msg):
    errors.append(msg)


lock = json.loads((root / "ci/locks/lock.json").read_text())
hex64 = re.compile(r"^[0-9a-f]{64}$")
hex40 = re.compile(r"^[0-9a-f]{40}$")

# ---- lock completeness --------------------------------------------------------
if not hex40.match(lock["upstream"]["commit"]):
    err("upstream.commit must be a full 40-hex commit")
if not hex64.match(lock["go"]["linux_amd64"]["sha256"]):
    err("go.linux_amd64.sha256 must be 64 hex")
for series, s in lock["openwrt"].items():
    if s["package_format"] not in ("ipk", "apk"):
        err(f"openwrt.{series}.package_format")
    for name, f in s["feeds"].items():
        if not hex40.match(f["commit"]):
            err(f"openwrt.{series}.feeds.{name}.commit must be a full commit")
    for tgt, t in s["targets"].items():
        if not hex64.match(t["sdk_sha256"]):
            err(f"openwrt.{series}.{tgt}.sdk_sha256 must be 64 hex")
        for floating in ("latest", "snapshots", "master", "testing"):
            if floating in t["sdk_url"].split("/"):
                err(f"openwrt.{series}.{tgt}.sdk_url uses floating '{floating}'")
        if s["release"] not in t["sdk"]:
            err(f"openwrt.{series}.{tgt}.sdk does not match release {s['release']}")
for name, a in lock["actions"].items():
    if not hex40.match(a["sha"]):
        err(f"actions.{name}.sha must be a full commit SHA")

# ---- workflows are pinned -----------------------------------------------------
known = {name: a["sha"] for name, a in lock["actions"].items()}
for wf in sorted((root / ".github/workflows").glob("*.yml")):
    for n, line in enumerate(wf.read_text().splitlines(), 1):
        m = re.search(r"\buses:\s*([^\s#]+)", line)
        if not m:
            continue
        ref = m.group(1)
        if ref.startswith("./"):
            continue
        name, _, sha = ref.partition("@")
        if not hex40.match(sha):
            err(f"{wf.name}:{n}: '{ref}' is not pinned to a full commit SHA")
        elif name in known and known[name] != sha:
            err(f"{wf.name}:{n}: {name} SHA differs from ci/locks/lock.json")
        elif name not in known:
            err(f"{wf.name}:{n}: {name} is not recorded in ci/locks/lock.json")
    text = wf.read_text()
    if "pull_request_target" in text:
        err(f"{wf.name}: pull_request_target must not be used (untrusted code with secrets)")
    if re.search(r"continue-on-error:\s*true", text) and wf.name in ("release.yml",):
        err(f"{wf.name}: continue-on-error would let a release ship without Full")

# ---- Full is the reviewed upstream set ----------------------------------------
upstream_tags = sorted((root / "release/DEFAULT_BUILD_TAGS").read_text().strip().split(","))
locked_tags = sorted((root / "ci/locks/full-tags.txt").read_text().split())
if upstream_tags != locked_tags:
    err("ci/locks/full-tags.txt differs from release/DEFAULT_BUILD_TAGS: review the diff, then update both lock and features.expected.json")
expected = json.loads((root / "ci/locks/features.expected.json").read_text())
for t in locked_tags:
    if t not in expected["required_build_tags"]:
        err(f"features.expected.json lacks required build tag {t}")

# ---- go.mod vs locked toolchain ----------------------------------------------
gomod = (root / "go.mod").read_text()
m = re.search(r"^go\s+(\S+)", gomod, re.M)


def vt(v):
    return tuple(int(x) for x in v.split("."))


if not m or vt(m.group(1)) != vt(lock["upstream"]["go_mod_go"]):
    err(f"go.mod go directive {m.group(1) if m else '?'} != lock upstream.go_mod_go {lock['upstream']['go_mod_go']}")
if vt(lock["go"]["version"]) < vt(lock["upstream"]["go_mod_go"]):
    err("locked Go toolchain is older than go.mod requires")

# ---- sample configuration validates under both profiles -----------------------
for tag in ("mw_lite", ""):
    cmd = ["go", "run"] + (["-tags", tag] if tag else []) + ["./cmd/swanix", "check", "-c", "openwrt/swanix-common/files/etc/config/swanix"]
    r = subprocess.run(cmd, cwd=root, capture_output=True, text=True)
    if r.returncode != 0:
        err(f"sample config rejected by {'lite' if tag else 'full'} build: {r.stdout}{r.stderr}")

if errors:
    print("validate: FAILED", file=sys.stderr)
    for e in errors:
        print(" -", e, file=sys.stderr)
    sys.exit(1)
print("validate: ok")
