#!/usr/bin/env python3
"""Boot a stock OpenWrt x86-64 image in QEMU and run guest.sh inside it.

Usage: run.py <series> <packages-dir> <work-dir> [--timeout SECONDS] [--arm64 PACKAGE_ARCH] [--host-step CMD]

--host-step runs CMD on the host when the guest asks for it (it prints `VMTEST-HOST-STEP: <name>` and waits
for the answer). VM_BASE_URL points at the guest's web server (host port 18080 -> guest 192.168.1.1:80).

--arm64 boots the armsr/armv8 image under QEMU TCG (no KVM on an x86 host, so it is slow) and adds
PACKAGE_ARCH (e.g. aarch64_cortex-a53, the mediatek/filogic package arch) to the guest's opkg
architecture list, so the filogic packages install on the generic arm64 userland.

The VM runs the real netifd, procd, ubus/rpcd, hotplug and firewall4. It reaches the
package feeds through QEMU user networking; the packages under test and guest.sh arrive on
a second disk. Everything the guest prints is relayed; the run passes when guest.sh prints
`VMTEST-RESULT: PASS`. KVM is used when /dev/kvm is usable, otherwise TCG (slower, still
boots in under a minute).
"""
import hashlib
import json
import re
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.request

root = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../.."))
lock = json.load(open(os.path.join(root, "ci/locks/lock.json")))


def main():
    series, pkgdir, work = sys.argv[1], os.path.abspath(sys.argv[2]), os.path.abspath(sys.argv[3])
    timeout = 1500
    if "--timeout" in sys.argv:
        timeout = int(sys.argv[sys.argv.index("--timeout") + 1])
    arm = sys.argv[sys.argv.index("--arm64") + 1] if "--arm64" in sys.argv else None
    t = lock["openwrt"][series]["vm_arm64"] if arm else lock["openwrt"][series]["targets"]["x86/64"]
    os.makedirs(work, exist_ok=True)

    gz = os.path.join(work, os.path.basename(t["image_url"]))
    if not os.path.exists(gz):
        urllib.request.urlretrieve(t["image_url"], gz)
    h = hashlib.sha256(open(gz, "rb").read()).hexdigest()
    if h != t["image_sha256"]:
        sys.exit(f"image hash mismatch: {h} != {t['image_sha256']}")
    disk = os.path.join(work, "openwrt.img")
    # the published image has trailing garbage after the gzip stream: let gunzip tolerate it
    subprocess.run(f"gunzip -c {gz} > {disk}", shell=True, check=False, stderr=subprocess.DEVNULL)

    # second disk: the packages under test plus the guest script
    stage = os.path.join(work, "stage")
    shutil.rmtree(stage, ignore_errors=True)
    os.makedirs(os.path.join(stage, "pkgs"))
    for f in os.listdir(pkgdir):
        if f.endswith((".ipk", ".apk")):
            shutil.copy(os.path.join(pkgdir, f), os.path.join(stage, "pkgs", f))
    shutil.copy(os.path.join(os.path.dirname(__file__), "guest.sh"), os.path.join(stage, "guest.sh"))
    open(os.path.join(stage, "series"), "w").write(series + "\n")
    # the test ISP's IPv6 side (RAs + DHCPv6 with prefix delegation), built for the guest
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="arm64" if arm else "amd64")
    subprocess.run(["go", "build", "-trimpath", "-o", os.path.join(stage, "isp6"), "./ci/test/vm/isp6"], cwd=root, env=env, check=True)
    if arm:
        open(os.path.join(stage, "extra_arch"), "w").write(arm + "\n")
    disk2 = os.path.join(work, "data.img")
    subprocess.run(["truncate", "-s", "256M", disk2], check=True)
    subprocess.run(["mke2fs", "-q", "-t", "ext4", "-F", "-d", stage, disk2], check=True)

    sock = os.path.join(work, "serial.sock")
    if os.path.exists(sock):
        os.unlink(sock)
    nic = "user,model=virtio-net-pci,net=192.168.1.0/24,host=192.168.1.2,dns=192.168.1.3,hostfwd=tcp:127.0.0.1:18080-192.168.1.1:80"
    if arm:
        accel = "tcg"
        vars_fd = os.path.join(work, "vars.fd")
        subprocess.run(["truncate", "-s", "64M", vars_fd], check=True)
        cmd = ["qemu-system-aarch64", "-M", "virt", "-cpu", "cortex-a57", "-m", "1024", "-smp", "4", "-display", "none", "-monitor", "none",
               "-drive", "if=pflash,format=raw,readonly=on,file=/usr/share/AAVMF/AAVMF_CODE.fd",
               "-drive", f"if=pflash,format=raw,file={vars_fd}",
               "-drive", f"file={disk},format=raw,if=virtio", "-drive", f"file={disk2},format=raw,if=virtio",
               "-nic", nic, "-serial", f"unix:{sock},server,nowait"]
    else:
        accel = "kvm" if os.access("/dev/kvm", os.R_OK | os.W_OK) else "tcg"
        cmd = ["qemu-system-x86_64", "-accel", accel, "-m", "768", "-smp", "2", "-display", "none", "-monitor", "none",
               "-drive", f"file={disk},format=raw,if=virtio", "-drive", f"file={disk2},format=raw,if=virtio",
               "-nic", nic, "-serial", f"unix:{sock},server,nowait"]
    print(f"[vm] {series}: booting with {accel}", flush=True)
    qemu = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        s = None
        for _ in range(120):  # qemu creates the serial socket a moment after it starts
            if qemu.poll() is not None:
                print("[vm] qemu exited before the serial console was available", file=sys.stderr)
                return 1
            try:
                s = socket.socket(socket.AF_UNIX)
                s.connect(sock)
                break
            except OSError:
                s.close()
                s = None
                time.sleep(0.5)
        if s is None:
            print("[vm] serial console never appeared", file=sys.stderr)
            return 1
        s.settimeout(2)
        buf = b""
        t0 = time.time()
        sent = False
        result = None
        host_step = sys.argv[sys.argv.index("--host-step") + 1] if "--host-step" in sys.argv else None
        step_done = 0
        while time.time() - t0 < timeout:
            try:
                d = s.recv(65536)
            except socket.timeout:
                d = b""
            if d:
                sys.stdout.write(d.decode("utf8", "replace"))
                sys.stdout.flush()
                buf += d
            if not sent and b"Please press Enter to activate this console" in buf:
                s.send(b"\n")
                time.sleep(2)
                # mount the data disk and run the guest script; its output goes to the console
                s.send(b"mkdir -p /mnt/data && mount /dev/vdb /mnt/data && sh /mnt/data/guest.sh; echo VMTEST-EXIT=$?\n")
                sent = True
            m = list(re.finditer(rb"^VMTEST-HOST-STEP: (\w+)", buf, re.M))
            if len(m) > step_done:
                step_done = len(m)
                if host_step:
                    print(f"\n[vm] host step: {host_step}", flush=True)
                    env = dict(os.environ, VM_BASE_URL="http://127.0.0.1:18080", SWANIX_UI_APPLY="1")
                    rc = subprocess.run(host_step, shell=True, env=env).returncode
                    s.send(b"HOSTDONE\n" if rc == 0 else b"HOSTFAIL\n")
                else:
                    s.send(b"HOSTSKIP\n")
            if b"VMTEST-RESULT: PASS" in buf:
                result = True
            if re.search(rb"VMTEST-EXIT=\d+", buf):
                break
        else:
            print("\n[vm] timed out", file=sys.stderr)
            return 2
        if result:
            print("\n[vm] guest reported PASS")
            return 0
        print("\n[vm] guest did not report PASS", file=sys.stderr)
        return 1
    finally:
        qemu.kill()


sys.exit(main())
