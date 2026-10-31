# Architecture

```
LAN traffic     -> nft prerouting (mangle-1) -> select_lan  -> ct mark + packet mark -> ip rule -> WAN table -> fw4 NAT
router traffic  -> nft output (route hook)   -> select_local-> same
probes          -> SO_MARK (probe bit) + SO_BINDTODEVICE -> probe rule -> that WAN's probe table (never falls over)
Full proxy      -> sing-box dial             -> wan_policy picks a WAN right before connect (SO_MARK + source bind)
```

* Own objects only: `table inet swanix`, `ip rule` priorities `priority_base … +255`, routing tables `table_base+id`
  (business) and `table_base+256+id` (probe), the mark bits of `kernel.mark_mask`. Nothing else is flushed.
* Mark window (8 bits inside `mark_mask`): bits 0–3 WAN ID (0 unassigned, 1–14 WANs, 15 reject), bit 4 inbound,
  bit 5 router-originated, bit 6 probe, bit 7 reserved.
* A connection is placed once, on its first packet: `prerouting` (and `output`) jump to `prerouting_new`
  (`output_new`) only for `ct state new`. That chain marks connections arriving from a WAN as inbound, skips local and
  bypass destinations, and selects a line: `numgen random mod 100 vmap @pol_<policy>_<4|6>` → `jump sel_wanN`, which
  sets ct mark and packet mark (other components' bits preserved). Weights are an interval verdict map; updating it
  is one atomic `flush map` + `add element` batch.
* Every packet (fast path): `ct direction . ct mark & (ID | inbound) vmap @pm_map` copies the connection's line into
  the packet mark with one lookup; keys that are not in the map (unplaced connections, replies of LAN connections,
  the original direction of inbound connections) fall through unmarked. No interface, FIB or bypass lookup happens
  after a connection's first packet; measured overhead against a path without swanix is within noise
  (see PERFORMANCE.md).
* Stickiness: per-line dynamic sets keyed on the source (or source . destination); a new connection reuses a line
  only while that line has a share, and refreshes its entry (`update`), so it expires `affinity_timeout` after the
  client's last new connection.
* Direction: original-direction packets of LAN/local connections get the WAN mark; for connections that *arrived*
  from a WAN (port forwards, router services) only the reply direction does, so the first inbound packet is
  routed normally and the reply leaves through the arrival WAN. The output hook always exists: with
  `manage_router_traffic 0` it only does this reply marking, so services on the router stay reachable over every line.
  ICMP errors about a tracked connection (RELATED) carry that connection's ct mark and take the same path; both the
  forwarded and the router-generated cases are covered by the namespace test.
* Routing order: probe rules (`base+10+id`) → `lookup main suppress_prefixlength 0` (`base+100`, so connected/VPN
  routes win and default routes do not) → business rules (`base+200+id`) → reject (`base+250`, `unreachable`).
  Every WAN table ends with `unreachable default metric 4278198272`: a lookup that fails terminates there instead of
  falling through to `main`.
* Start order: routes → rules → nft on first apply; incremental updates keep the same order.
* Health: per WAN and family, rounds of parallel probes (`successes_required` independent targets per round),
  `down_rounds` / `up_rounds` consecutive rounds, `recover_ramp` before full share. Link events are immediate;
  link-up alone never makes a WAN usable.
* The daemon stopping does not remove kernel state (established connections keep their path); health scheduling is
  paused until it returns. `swanix remove` / `swanix rollback` withdraw everything it owns.
