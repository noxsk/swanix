# Status: what exists, what was verified, what was not

Dates and numbers below come from runs in the development environment (Linux 6.18 micro-VM, root, no IPv6 in
the kernel, no KVM). "Verified" means a test executed and passed there. Nothing here is a stability claim.

## Implemented

| Area | State |
|---|---|
| UCI parser/decoder, strict validation with field-path errors, Lite/Full profile check | done, unit tested |
| Health state machine (per WAN, per family), IP-literal TCP/HTTP(S) probers with `SO_MARK` | done, unit tested |
| Scheduler (capacity × preference × utilisation × latency × probe failures, EWMA, min share, 10-point step, 3-point deadband, immediate zero on down, recovery ramp, stale-metric fallback) | done, unit tested |
| nftables ruleset renderer (connection-level selection, inbound-direction tracking, bypass sets, NAT66), `ip rule`/route reconciler, ownership-scoped removal, `rp_filter` loosen/restore on WAN devices only | done, loaded into a real kernel |
| Incremental updates (sets + selection chains + weight maps in one atomic batch; counters and conntrack untouched) | done, verified |
| Engine (probe/sample/schedule/reconcile loops), netifd snapshot (ubus), netlink change watcher, control socket, CLI | done |
| `apply` / `confirm` / `rollback` with out-of-process guard | done; unit tested, and driven end to end through the real CLI in namespaces with fake init scripts |
| `doctor` (fw4 masq coverage, flow offload, mark overlap, priority range, mwan3, rp_filter, duplicate addresses) | done, unit tested |
| Lite entry (`mw_lite`), `go list -deps` gate | done, verified |
| Full entry: `multiwan` service, `wan_policy` dialer hook, `proxy.json` injection, feature report from live registries | done, verified in namespaces |
| OpenWrt packages (`swanix-common`, `swanix-lite`, `swanix-full`, `luci-app-swanix` + zh_Hans), init, hotplug, rpcd plugin, ACL | done; ipk built with the real 24.10.8 SDK |
| LuCI views (overview, lines, policies, health, IPv6, proxy, system) | rendered in headless Chromium with LuCI's own luci.js/ui.js/form.js against a stubbed ubus backend (`ci/test/luci/render.js`); **never opened on a router** |
| CI workflows, locks, build/sign/release scripts | ci and build workflows run green on GitHub (see below); the release workflow (signing, publishing) has **never run** |

## Verified in a real OpenWrt VM (`ci/test/vm`, QEMU, stock 24.10.8 and 25.12.6 x86-64 images)

The VM runs the real netifd, procd, ubus/rpcd, hotplug and firewall4. A LAN client and two ISPs are namespaces wired to
netifd-managed interfaces (static IPv4 + IPv6 WANs, a LAN bridge), the packages come from the SDK builds and install through
opkg / apk with their real dependencies. Passed on both series, locally under TCG and on GitHub Actions with KVM (about 2.7 minutes per series; the same script is the CI job):

* install leaves swanix disabled and observe-only; `swanix check`; `swanix doctor` passes against the real fw4/netifd state;
* `apply` through the real procd init + `confirm`; status over `ubus call swanix status` (the rpcd plugin);
* the **real LuCI** (uhttpd + rpcd + luci-base + luci-mod-network, theme bootstrap) driven by a headless browser from the host:
  login, the menu entry under Network, and all six pages of the Lite build render live data from the real backend without
  JavaScript or HTTP errors (`ci/test/luci/vm-browser.js`; screenshots are CI artifacts). The same browser also edits a health
  setting, presses Save & Apply and Confirm on the takeover dialog, and the guest then checks that the change was
  committed, no apply is pending and the daemon runs with it (verified locally on 24.10 and by the `build` workflow's VM jobs on 24.10 and 25.12; the arm64 VM job has no browser
  and skips this step). On a failed pre-apply check the UI calls the rpcd `restore` method to put back the saved
  configuration; when swanix is not enabled/managed, `apply` just restarts or stops the daemon and skips the confirm window;
* IPv4: 120 connections split 71–85 / 35–49 over the two lines (shares 63/37);
* upstream of a line dies → down in a few probe rounds and 30/30 new connections on the other line; recovery;
  `ifdown`/`ifup` of a WAN through netifd → down within 15 s, back healthy; `firewall reload` keeps both nft tables and traffic;
* IPv6 **NAT66**: 80 connections split over both lines through source translation;
* IPv6 **native**: netifd delegates a /56 per WAN and assigns a /64 of each to the LAN; a client bound to each prefix leaves
  through that line (20/20); with a line down its prefix is rejected, not rerouted; the other prefix is unaffected;
* no confirmation → the guard withdraws table, rules and service, restores the configuration and a late `confirm` fails;
* **software flow offloading** (section 7d of the VM test, see "Not verified" for hardware offloading);
* **dynamic uplinks**: wan1 switched to DHCPv4 + a `wan6` DHCPv6 section (address, `/56` prefix delegation, RAs), wan2 to
  **PPPoE** (`ipv6 auto`, so netifd's `wan2_6` gets DHCPv6-PD over the session). The ISP side is a second dnsmasq
  (DHCPv4), an rp-pppoe access concentrator (kernel mode) and `ci/test/vm/isp6`, a small RA + DHCPv6 (IA_NA + IA_PD)
  responder written for the test because ISC dhcpd refuses PPP devices and Kea binds the PPP peer's address. IPv4 is
  balanced over the DHCP and the PPPoE line; native IPv6 sends each delegated prefix out of its own line (20/20).
  The ISP **drops the PPPoE session** and comes back with a new pool and prefix: wan2 is down within seconds, all new
  connections use wan1 meanwhile, then wan2 is healthy again with the new address in its table and the new prefix
  selected; the withdrawn prefix no longer gets out. The ISP **renumbers its DHCPv4 pool**: the renewal is refused,
  the new lease appears in wan1's table and both lines carry traffic. The ISP **delegates a different prefix**: netifd
  re-assigns the LAN `/64` and swanix selects by the new prefix;
* in-place reinstall (upgrade path) restarts the service and re-applies; removal withdraws the table, rules and daemon,
  leaves fw4 alone and keeps connectivity.

The same script also passed against an **arm64** OpenWrt 24.10.8 (`armsr/armv8` under QEMU TCG, kernel 6.6.144 aarch64) with
the packages built for `mediatek/filogic` (same arm64 musl binaries; their package arch is added to opkg's list because the
generic arm64 image uses `aarch64_generic`). That shows the arm64 build runs the whole data path; it says nothing about a
filogic board's drivers, offload or speed.

Defects this found and fixed: a probe-detected Down stayed in the weight map until the next scheduling tick; the removal
script lived in `swanix-common` and could run after the binary was gone; `ipv6_network auto` did not find netifd's `<network>_6`
interface, so a PPPoE line's IPv6 was never used. Not covered: hardware offload, real modems/BRAS (only the software
servers above), multi-hour behaviour.

## Verified on GitHub Actions (private repository, runs of this repo)

* `ci` (validate, core tests with `-race`, namespace integration of the kernel path, Lite daemon, apply/confirm/rollback,
  Full runtime with `wan_policy`, LuCI render): green.
* `build` (workflow_dispatch, no tag, nothing published): for the full matrix 24.10/25.12 × x86/64, mediatek/filogic × Lite/Full,
  the pinned SDK + feeds, the locked Go, and for Full the cronet toolchain (CGO + musl, upstream `release/DEFAULT_BUILD_TAGS`
  + `with_musl`) all built native ipk/apk packages. The feature gate passed on the amd64 binaries (Full: `cgo=1`,
  `with_naive_outbound`, `with_musl`, 76 proxy features; Lite: no proxy feature, tag `mw_lite`).
* `package-smoke` in the official x86-64 rootfs of each series, online: dependency resolution, `swanix-lite` ⇄ `swanix-full`
  mutual exclusion in both directions, config preserved across the switch, LuCI + zh_Hans packages, upgrade in place,
  removal. Passed for 24.10 (opkg) and 25.12 (apk).
* Measured sizes (x86_64, version 0.0.0_git20261027): Lite ipk 3.0 MB / binary 7.1 MB; Full ipk 32 MB / binary 90.6 MB.
  Full will not fit small-flash routers.
* Idle memory/CPU of those CI-built binaries (static musl, x86_64, namespaces on a micro-VM, one run, `TestMeasureResources`;
  both lines healthy, 35 s settle, then 30 s idle): Lite RSS 10.4 MiB, CPU 0.06 s; Full with `proxy.enabled=0` RSS 49.3 MiB,
  CPU 0.05 s; Full with a SOCKS inbound + `direct` outbound RSS 52.7 MiB, CPU 0.07 s. So Full costs about 39 MiB more than Lite
  even when the proxy is off (the upstream registries and dependencies are initialised at start). The 1500-connection burst
  in that test goes through the kernel path, not through the proxy, so it says nothing about proxy throughput. Not measured:
  routers and behaviour under many concurrent proxy flows.
* Bulk download of 1 GiB over one TCP connection (`TestMeasureThroughput`, CI-built Full, veth links in a micro-VM, Python
  endpoints, two runs each): through the kernel path 7.2–8.6 Gbit/s with **0.00 s** daemon CPU (the daemon is not in the data
  path); through the proxy runtime (SOCKS + `direct`) 10.5–12.1 Gbit/s with **0.52–0.57 s** daemon CPU per GiB. The Python
  endpoints and the NAT/conntrack cost of the kernel path dominate these figures, so they show that the proxy path costs about
  half a CPU-second per GiB here, not which path is faster on a router.
* Forwarding path (2026-10-31): decisions run on a connection's first packet only, every later packet costs one map
  lookup; paired overhead against the same path without swanix is −1.8% connections/s and −0.8% throughput on a 4-vCPU
  VM, −5.4% / −5.4% on a 2-vCPU runner. Sticky entries are now refreshed on every new connection (they used to expire
  `affinity_timeout` after the first one, moving active clients); a kernel test covers it. `apply --verify` sets the
  verification budget (default 40 s) for slow systems. The `build` workflow (all packages, x86 and arm64 VMs) passed
  with these changes.
* `perf` (manual): data-path overhead against a baseline without swanix, balanced and sticky connection rates,
  failover convergence, weight updates under load, Full kernel path vs TUN, control-plane benchmarks. Results from the
  development VM and a 2-vCPU runner are in [PERFORMANCE.md](PERFORMANCE.md); no router hardware has been measured.
* Not run on GitHub: the `release` workflow (signing, publishing) — deliberately.

## Verified locally (executed)

* `go test -race ./experimental/multiwan/...`: config, health, scheduler, kernel rendering, engine, session, doctor, control, netifd parsing.
* Cross-connection stickiness (`affinity source` / `source_destination`, `affinity_timeout`): per-line dynamic nft sets
  remember a client's line; a client stays on it across connections, 16 clients still spread over both lines, and a client
  whose line leaves the pool moves at once. `source_destination` carried traffic too: 10 clients × 2 destinations, every
  (client, destination) pair stayed on one line over 10 connections, and 5 of the 10 clients reached the two
  destinations over different lines. The NAT66 variants are rendered and loaded into the kernel only.
* Saturation spill-over (`policy.saturation_threshold`, optional): both lines configured at 4 Mbit/s, 20 Mbit/s of
  traffic arriving on wan1 (`TestLiteSaturationSpillover`, Lite daemon, real interface counters): wan1 was reported
  saturated within a few seconds and got 1 of the next 100 connections; after the load stopped it was released and its
  share ramped back. The decision uses the daemon's measured rates (sampled every `sample_interval`, smoothed) at the
  next scheduling tick (`update_interval`), so it reacts in seconds, not instantly, and it moves new connections only.
* Kernel data path in network namespaces (two ISPs + LAN, real TCP): per-connection split follows the weights
  (70/30 → within ±10 points over 300 connections); weight updates do not move established connections; empty pool
  rejects instead of leaving through `main`; router-originated traffic follows the weights; ICMP errors RELATED to an
  inbound flow (the LAN host's port-unreachable for a forwarded UDP port, the router's own port-unreachable) and a TCP
  service on the router answer through the arrival line while `main` points at the other one, with
  `manage_router_traffic` on and off (with it off this used to leave through `main`: fixed); probe-marked sockets stay
  on their WAN and fail rather than fall over to another one; a port-forward replies through the arrival WAN even when
  `main` points elsewhere; counters equal observed assignments; removal leaves foreign nft tables/rules untouched and
  restores `rp_filter`.
* Lite daemon (real binary, `ubus` stub): both lines healthy, balanced; `ip link set down` on one WAN is picked up from
  netlink (no probe rounds needed) and the line leaves the pool, link up alone does not bring it back, probes do;
  upstream of one line dies → out of the pool in
  about 3 s with probes `interval=1 down_rounds=2`, traffic continues on the other line, recovery ramps back in,
  SIGTERM keeps the kernel state, `swanix remove` withdraws it.
* Full runtime (default tags, no cronet): SOCKS inbound + `direct` outbound with `wan_policy` balances per connection and
  follows failover. A non-existent policy is rejected by `swanix check`; `wan_policy` with `detour`, `bind_interface`,
  `routing_mark`, bind addresses or `netns` is rejected by the dialer.
* Full compiles with all of upstream's `release/DEFAULT_BUILD_TAGS` (purego variant, CGO off): 76 proxy features reported.
* The locked Go (1.26.10) builds Lite for amd64 and arm64 via `ci/build/binary.sh`.
* Native **ipk** packages built with the real OpenWrt 24.10.8 x86/64 SDK (feeds pinned to the locked commits);
  `swanix-common` + `swanix-lite` + `luci-app-swanix` installed with `opkg` in the 24.10.8 rootfs: files, default
  disabled/observe state, `swanix check/version/features` run, config survives remove + reinstall, removal keeps a
  modified conffile. (Run offline with `--force-depends`; dependency resolution and the lite⇄full conflict are
  exercised only by the CI variant of the smoke test.)
* `apply` → `confirm`: mwan3 (fake) stopped before swanix starts, kernel objects installed, a second `apply` refused during
  the window, boot enablement switched on confirm, pending state cleared, the guard does not undo a confirmed switch.
  `apply` without `confirm`: the guard (a separate process) withdraws the nft table, stops swanix, restores mwan3 and the
  saved configuration at the deadline; a late `confirm` fails. This test found and fixed a real defect: the guard could
  start before the pending state was written and then exit immediately (it failed about one run in three before the fix,
  0 of 6 after). A second defect surfaced on the GitHub runner: a guard rolling back at its deadline signalled itself
  (`KillGuard` on its own PID) before it had removed the state, so the rollback sometimes stopped half way; now covered by a
  unit test and fixed. Not covered: verification failure after takeover (the 40 s probe wait) and a crashed daemon.
* The rpcd plugin ran for real in the 24.10.8 rootfs with its own `jshn`: `list`, `version`, `status` (daemon down →
  `ok:false`), `check`, `doctor`, `prepare`, unknown method, and a shell-metacharacter `timeout` argument (ignored; nothing executed).
  On the stock image `ip` is BusyBox, so `doctor` correctly fails `tool:ip`; the package depends on `ip-full`.
* All seven LuCI views render the fixture data without page errors (Lite profile skips the proxy page).
* The SDK rejected a glibc-linked Full binary: the package gate that keeps glibc out of OpenWrt packages works.

## Not verified / not implemented

* Save & Apply in a browser on arm64 (the arm64 VM runs without a host browser). Everything else of the router stack is covered by the VM tests above (x86-64 and arm64 userland, QEMU); no real-hardware run.
* Full (musl + CGO + cronet) builds and installs in CI. The CI-built binary carried real **naive** (cronet) traffic in
  namespaces (`TestFullNaiveWANPolicy`, naive server = a plain sing-box in each ISP namespace): with wan1 as backup, 30 of 30
  proxied connections left through wan2, i.e. cronet's outer connection is dialled through `wan_policy`; after wan2 died
  the next session came up on wan1 (it took about 22 s, the time cronet needs to notice the dead HTTP/2 session and
  redial). As documented, one naive session carries many streams, so they share one line.
* apk has no `Conflicts` field in these packages: exclusion of the two cores comes from both providing
  `swanix-core=<same version>`, which apk treats as a conflict (confirmed by the CI smoke test).
* **mediatek/filogic hardware**: the arm64 binaries and packages run in an arm64 VM (see above) but not on a board.
* Flow offloading: **software** offloading is supported when declared (`global.flow_offload software` together with
  fw4's `flow_offloading 1`; `doctor` fails on a mismatch). In the VM all 12 parallel 20 MB downloads were offloaded
  (`[OFFLOAD]` in conntrack), completed over both lines, new connections stayed balanced and failover still moved
  them: an offloaded flow keeps the route of its first packets, which swanix chose. **Hardware** offloading
  (`flow_offloading_hw`) stays refused: it needs a board to verify.
* TUN takeover (`proxy.transparent_mode tun`, Full): the `tun` inbound in `proxy.json` with `auto_route` takes the LAN
  traffic and outbounds with `wan_policy` choose the line. `check`/`run` hold `proxy.json` to swanix's resources: the
  TUN's rule range must sit before `kernel.priority_base`, its table outside swanix's tables, `route.default_mark` and
  every `routing_mark` outside `kernel.mark_mask`, and at least one outbound must use `wan_policy`; a TUN with
  `auto_route` but `transparent_mode off` is refused. Verified in namespaces (`TestFullTUNTakesLANTraffic`): a LAN client
  routed to a wan2-pinned outbound left only through wan2 (the kernel path alone would have balanced it, so the traffic
  went through the TUN), another client was balanced by the proxy and moved to wan1 when wan2 died, router traffic kept
  working. That test found a defect: probes only carried their mark, so the TUN's "from 0.0.0.0 iif lo" rule (which comes
  before swanix's rules) took them into the proxy, where they succeeded over the other line and a dead line never went
  down; probes are now bound to their WAN's device as well. `auto_redirect` is **refused**: its nftables redirect only
  exempts packets whose mark equals its own output mark, so probes and `wan_policy` sockets were pulled back into the
  proxy (lines never became healthy). **TProxy** stays refused: it needs nft rules and ip rules the user maintains,
  which swanix cannot check. Not run on a router.
* Connection pools/mux: `wan_policy` selects the WAN when a *physical* socket is created. A long-lived mux/HTTP2/QUIC
  session therefore stays on the WAN it was dialled on; it is not spread over WANs. When that WAN goes down its sockets
  are closed (`proxy.interrupt_on_down`, default on) so the session redials elsewhere: in namespaces a SOCKS session held
  through wan2 ended 3–4 s after isp2 was blackholed (probes `interval 1`, `down_rounds 2`); with the option off it was
  still hanging 25 s later. Unconnected UDP sockets (`ListenPacket`, used by QUIC-based outbounds) now get the line's
  mark as well; before, they bypassed `wan_policy`.
* swanix never flushes conntrack (no `conntrack -F`). When a line's address goes away the kernel's masquerading drops
  the connections translated to that address (checked in the VM after the DHCP renumbering); connections of a line that
  goes down without losing its address keep their mark and time out or are retried by the application.
* DHCP renewal, PPPoE redial and PD updates were exercised against software servers in the VM (above), not against real
  ISP equipment.
* Lite vs Full RSS/CPU/size comparison on target hardware: not measured. Measured sizes of package files go into
  `size-report.json` at release time; nothing is promised in advance.
* Unified `/etc/swanix/proxy.json` merging is limited to injecting the `multiwan` service (duplicate type/tag
  rejected); the user's file is never rewritten.

## Design decisions that differ from the first sketch

* `startup_mode=observe` has no probes: probing needs routing tables, which would be kernel writes. Health in
  observe mode is netifd link state only and says so in `reason`.
* Prebuilt-binary packaging: both variants are compiled with the locked Go outside the SDK and the SDK is the
  final packager (ipk/apk, metadata, indexes). `openwrt/swanix/Makefile` also compiles from source with the
  buildroot's Go when `SWANIX_PREBUILT` is not set.
* `CONFLICTS` is declared only on `swanix-lite` (declaring it on both makes Kconfig report a recursive dependency).
* Probe targets must be IP literals so a name lookup can never be answered through another WAN.
