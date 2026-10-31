# Operations

> 中文使用说明见 [USAGE.md](USAGE.md)，完整配置参考见 [CONFIG.md](CONFIG.md)。This page is the short English summary.

## Configuration (`/etc/config/swanix`)

The only source of truth for multi-WAN settings. Installed with `enabled 0` and `startup_mode observe`.
Unknown fields, duplicate WAN IDs, missing references, non-positive capacities and the like are rejected with a
field path, a reason and an action, for example:

```
wan.wan2.upload_mbps: 必须大于 0，单位 Mbps
proxy.enabled: 当前 lite 构建不含代理模块；改用 Full 构建，或将其设为 0
```

Capacities (`download_mbps`, `upload_mbps`) are the lines' usable rates. The values shipped in the file are examples.
`preference` is relative (default 100), not a capacity. `role backup` lines take traffic only when no active line is usable.
`affinity` is `connection` (default, best balance), `source` or `source_destination`; sticky modes remember a client's
line for `affinity_timeout` seconds and reuse it only while that line is in the pool.
`unavailable_action` is `reject`: an empty pool never falls back to `main`.
`saturation_threshold` (optional, 50–99, off by default) is a percentage of a line's configured capacity in its
`traffic_bias` direction: while a member carries at least that much and another member is below it, the busy line keeps
only the minimum share and new connections go to the others; it takes connections again once it is 10 points below
the threshold (ramping back), and with every member that busy the normal weights apply. Existing connections never
move, and a sticky client keeps its remembered line even when that line is saturated (stickiness wins). The rates come
from the interface counters, so `download_mbps`/`upload_mbps` must be the real usable rates for this to mean anything.

Full: proxy settings live in `/etc/swanix/proxy.json` (see `/usr/share/swanix/proxy.json.example`). Set
`"wan_policy": "<policy>"` on an outbound to choose a WAN per connection; it cannot be combined with `detour`,
`bind_interface`, `routing_mark`, `inet4_bind_address`, `inet6_bind_address` or `netns`.
A WAN is chosen when a socket is created: per connection for TCP and connected UDP, per socket for an
unconnected UDP socket (only the mark is set there; the line's table and masquerading choose the source). With
`proxy.interrupt_on_down 1` (default) the sockets dialled through a line are closed when it goes down, so mux, HTTP/2
and QUIC sessions reconnect over a healthy line at once instead of waiting minutes for their own timeouts. Set it to `0`
to keep them open (useful when probes are unreliable and a "down" line may in fact still carry traffic).

TUN takeover: set `proxy.transparent_mode tun` and add a `tun` inbound with `auto_route` (not `auto_redirect`) to
`proxy.json`; keep its default rule index (9000) and table (2022) or choose others before `kernel.priority_base` and
outside swanix's table range, and give the outbounds `wan_policy`. `swanix check` refuses combinations that would
collide. Health probes are bound to their line's device, so policy-routing rules placed before swanix's (a TUN, a
VPN) cannot take them over another line.

## Uplink types

`wan.<name>.network` names a netifd interface of any protocol: static, DHCP, PPPoE. For IPv6, `ipv6_network auto`
(default) uses the WAN's own interface if it carries an IPv6 default route, else `<network>6` (the usual `wan6`
DHCPv6 section) or `<network>_6` (the interface netifd creates for PPPoE/DHCP with `option ipv6 auto`); set it
explicitly when more than one exists. Address changes, DHCP renewals, PPPoE redials and new delegated prefixes are
picked up from netifd and netlink without a restart. swanix flushes no conntrack state: when an address goes away the
kernel's masquerading drops the connections translated to it, and connections of a line that went down are not moved.

## Taking over from mwan3

1. `swanix check`, `swanix doctor` (fix every `fail`), `swanix preview`.
2. Set `global.enabled 1`, `startup_mode manage`, then `swanix apply`. It saves the configuration and mwan3's
   running/enabled state, arms the guard, stops mwan3, starts swanix, waits for a real probe to pass, and starts the
   confirmation window.
3. `swanix confirm` within the window (LuCI shows a countdown). Otherwise everything swanix installed is withdrawn,
   the saved configuration is restored and mwan3 is started again if it was running.

The package never disables mwan3 by itself. `observe` can run next to mwan3.

## DNS and other traffic that must keep a fixed line

The router's own connections are balanced like any other. An ISP's resolver often refuses queries from another ISP's
address, so pin DNS (and DDNS, VPN control channels, NTP if you rely on a provider's server) with a rule that applies to
router-originated traffic only:

```
config rule 'dns_wan1'
	option priority '10'
	option local '1'
	option proto 'udp'
	list dest_port '53'
	option policy 'wan1_only'      # a policy with the single member wan1

config policy 'wan1_only'
	list member 'wan1'
```

If that line fails the policy rejects new connections instead of falling back. To let the service fail over, add a
second member to the policy and give that WAN `role backup`: it is then used only while no active member is usable.

## Checks that matter

* firewall: the WAN zone needs `masq 1` without `masq_src`/`masq_dest` restrictions when `manage_router_traffic 1`,
  and flow offloading must match `global.flow_offload`: `off`, or `software` with fw4's `flow_offloading 1` (offloaded
  flows keep the line chosen for their first packets). Hardware offloading is refused. `doctor` fails otherwise.
* `rp_filter`: strict (1) on a managed WAN device is raised to loose (2) for that device only and restored on removal.
* Marks: `kernel.mark_mask` (default `0xff000000`) must not overlap other components; `doctor` warns when it finds a
  writer of those bits.

## Commands

`check`, `doctor [--json]`, `preview`, `run`, `status [--json]`, `history`, `apply [--timeout 90s]`, `confirm`,
`rollback`, `reconcile`, `features [--json]`, `version [--json]`, `remove`. `SWANIX_CONFIG` redirects the config path.

## Troubleshooting

* Traffic stopped after `apply`: wait for the window to expire or run `swanix rollback`.
* A line shows `down` while its modem is up: `swanix status` shows the reason; probe targets must be reachable
  through that line (they are routed by that line's probe table only).
* `reject` counter rising: no line is usable for that family; see per-line state.
