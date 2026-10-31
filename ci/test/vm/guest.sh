#!/bin/sh
# Runs inside the OpenWrt VM (see run.py). Plain ash; prints VMTEST-RESULT: PASS at the end.
D=/mnt/data
SERIES=$(cat $D/series)
say() { echo "VMTEST: $*"; }
fail() { echo "VMTEST-FAIL: $*"; echo "VMTEST-RESULT: FAIL"; exit 1; }

say "release: $(. /etc/openwrt_release; echo $DISTRIB_RELEASE) kernel $(uname -r)"

# --- 1. outbound access for installing packages: QEMU's user network is 192.168.1.0/24 with the
# gateway at .2 and DNS at .3, so the stock static LAN address (192.168.1.1) works unchanged
i=0; while [ $i -lt 90 ]; do ip -4 addr show br-lan 2>/dev/null | grep -q "inet 192.168.1.1/" && break; i=$((i+1)); sleep 1; done
ip route replace default via 192.168.1.2 || fail "cannot add the default route"
rm -f /etc/resolv.conf; echo "nameserver 192.168.1.3" > /etc/resolv.conf
i=0; while [ $i -lt 30 ]; do nslookup downloads.openwrt.org >/dev/null 2>&1 && break; i=$((i+1)); sleep 1; done
nslookup downloads.openwrt.org >/dev/null 2>&1 || { ip addr; ip route; fail "no outbound access"; }
say "outbound access ok"

# --- 2. package feeds over plain HTTP (signed indexes; the stock image has no CA bundle for its downloader)
if [ -f /etc/opkg/distfeeds.conf ]; then
	sed -i 's|https://downloads.openwrt.org|http://downloads.openwrt.org|' /etc/opkg/distfeeds.conf
	opkg update >/dev/null 2>&1 || fail "opkg update"
	INSTALL() { opkg install "$@"; }
else
	sed -i 's|https://downloads.openwrt.org|http://downloads.openwrt.org|' /etc/apk/repositories.d/*
	apk update >/dev/null 2>&1 || fail "apk update"
	INSTALL() { apk add --allow-untrusted "$@"; }
fi
if [ -f $D/extra_arch ] && [ -f /etc/opkg.conf ]; then
	# any "arch" line in opkg.conf replaces the built-in list, so repeat the effective one first
	opkg print-architecture | grep '^arch ' >> /etc/opkg.conf
	echo "arch $(cat $D/extra_arch) 10" >> /etc/opkg.conf
fi
# the ISP side of the dynamic-uplink tests needs a PPPoE access concentrator (section 7c)
INSTALL kmod-veth ip-full socat ppp-mod-pppoe rp-pppoe-server >/tmp/deps.log 2>&1 || { tail -20 /tmp/deps.log; fail "install kmod-veth ip-full socat rp-pppoe-server"; }
/etc/init.d/pppoe-server stop >/dev/null 2>&1; /etc/init.d/pppoe-server disable >/dev/null 2>&1

# --- 3. install the packages under test (dependencies come from the feeds)
if [ -f /etc/opkg/distfeeds.conf ]; then
	opkg install $D/pkgs/swanix-common_*.ipk $D/pkgs/swanix-lite_*.ipk >/tmp/install.log 2>&1 || { cat /tmp/install.log; fail "install"; }
else
	apk add --allow-untrusted $D/pkgs/swanix-common-*.apk $D/pkgs/swanix-lite-*.apk >/tmp/install.log 2>&1 || { cat /tmp/install.log; fail "install"; }
fi
INSTALL luci-base luci-mod-network luci-theme-bootstrap uhttpd uhttpd-mod-ubus rpcd-mod-luci >/tmp/luci-deps.log 2>&1 || { tail -15 /tmp/luci-deps.log; fail "install LuCI"; }
if [ -f /etc/opkg/distfeeds.conf ]; then opkg install $D/pkgs/luci-app-swanix_*.ipk $D/pkgs/luci-i18n-swanix-zh-cn_*.ipk >/tmp/luci-app.log 2>&1; else apk add --allow-untrusted $D/pkgs/luci-app-swanix-0*.apk $D/pkgs/luci-i18n-swanix-zh-cn-*.apk >/tmp/luci-app.log 2>&1; fi || { cat /tmp/luci-app.log; fail "install luci-app-swanix"; }
printf 'vmtest\nvmtest\n' | passwd root >/dev/null 2>&1
/etc/init.d/rpcd restart >/dev/null 2>&1
/etc/init.d/uhttpd restart >/dev/null 2>&1
ok() { echo "VMTEST-OK: $*"; }
[ -x /usr/bin/swanix ] || fail "swanix not installed"
ok "packages installed ($(swanix version | head -1))"
[ "$(uci -q get swanix.global.enabled)" = 0 ] && [ "$(uci -q get swanix.global.startup_mode)" = observe ] || fail "not disabled/observe after install"
ok "installed disabled and observe-only"

# --- 4. test topology: LAN client and two ISPs as namespaces, wired to real netifd interfaces
ip netns add lan; ip netns add isp1; ip netns add isp2
ip link add vl0 type veth peer name vl1 netns lan
ip link add vw1 type veth peer name vi1 netns isp1
ip link add vw2 type veth peer name vi2 netns isp2
ip link set vl0 up; ip link set vw1 up; ip link set vw2 up
nsx() { n=$1; shift; ip netns exec $n "$@"; }
for n in lan isp1 isp2; do nsx $n ip link set lo up; done
nsx lan ip link set vl1 up; nsx isp1 ip link set vi1 up; nsx isp2 ip link set vi2 up
# ISP side: the same "internet" addresses in both, so the answering banner shows the line that was used
nsx isp1 ip addr add 192.0.2.1/24 dev vi1;       nsx isp1 ip -6 addr add 2001:db8:a::1/64 dev vi1 nodad
nsx isp2 ip addr add 198.51.100.1/24 dev vi2;    nsx isp2 ip -6 addr add 2001:db8:b::1/64 dev vi2 nodad
for n in isp1 isp2; do nsx $n ip addr add 203.0.113.10/32 dev lo; nsx $n ip -6 addr add 2001:db8:ffff::10/128 dev lo nodad; done
nsx isp1 ip -6 route add 2001:db8:1::/48 via 2001:db8:a::2
nsx isp2 ip -6 route add 2001:db8:2::/48 via 2001:db8:b::2
printf '#!/bin/sh\necho wan1\n' > /tmp/b1.sh; printf '#!/bin/sh\necho wan2\n' > /tmp/b2.sh; chmod +x /tmp/b1.sh /tmp/b2.sh
start_servers() { # $1 = 1|2; the PIDs are kept so one ISP's servers can be stopped alone
	( exec ip netns exec isp$1 socat TCP4-LISTEN:80,bind=203.0.113.10,fork,reuseaddr SYSTEM:"echo wan$1" >/dev/null 2>&1 ) &
	echo $! >> /tmp/isp$1.pids
	( exec ip netns exec isp$1 socat TCP6-LISTEN:80,bind=[2001:db8:ffff::10],fork,reuseaddr,ipv6-v6only=1 SYSTEM:"echo wan$1" >/dev/null 2>&1 ) &
	echo $! >> /tmp/isp$1.pids
}
stop_servers() { for p in $(cat /tmp/isp$1.pids); do kill $p 2>/dev/null; done; : > /tmp/isp$1.pids; }
start_servers 1; start_servers 2
sleep 2

# netifd configuration: LAN bridge on the veth, two static WANs (IPv4 + IPv6), management NIC left alone
uci set network.@device[0].ports='vl0'
uci set network.lan.ipaddr='10.77.0.1'; uci set network.lan.netmask='255.255.255.0'
# eth0 keeps 192.168.1.1 as the management interface (LuCI is reached through it) and joins the lan firewall zone
uci set network.mgmt=interface; uci set network.mgmt.device=eth0; uci set network.mgmt.proto=static; uci set network.mgmt.ipaddr=192.168.1.1/24
uci add_list firewall.@zone[0].network=mgmt
uci -q delete network.lan.ip6assign; uci set network.lan.ip6addr='fd00:1::1/64'
uci -q delete network.wan6; uci -q delete network.wan
for spec in "wan vw1 192.0.2.2/24 192.0.2.1 2001:db8:a::2/64 2001:db8:a::1" "wan2 vw2 198.51.100.2/24 198.51.100.1 2001:db8:b::2/64 2001:db8:b::1"; do
	set -- $spec
	uci set network.$1=interface; uci set network.$1.device=$2; uci set network.$1.proto=static
	uci set network.$1.ipaddr=$3; uci set network.$1.gateway=$4; uci set network.$1.ip6addr=$5; uci set network.$1.ip6gw=$6
done
uci commit network
# firewall4: wan zone covers both lines, masquerade on (the default zone already has it)
uci set firewall.@zone[1].network='wan wan2'; uci set firewall.@zone[1].masq='1'; uci commit firewall
ip route del default 2>/dev/null
/etc/init.d/network reload >/dev/null 2>&1
/etc/init.d/firewall restart >/dev/null 2>&1
i=0; while [ $i -lt 90 ]; do ifstatus wan 2>/dev/null | grep -q '"up": true' && ifstatus wan2 2>/dev/null | grep -q '"up": true' && break; i=$((i+1)); sleep 1; done
ifstatus wan | grep -q '"up": true' && ifstatus wan2 | grep -q '"up": true' || { ifstatus wan; fail "netifd did not bring the WANs up"; }
nsx lan ip addr add 10.77.0.100/24 dev vl1; nsx lan ip route add default via 10.77.0.1
nsx lan ip -6 addr add fd00:1::100/64 dev vl1 nodad; nsx lan ip -6 route add default via fd00:1::1
sleep 3
ok "netifd: wan=$(ifstatus wan | jsonfilter -e '@.l3_device') wan2=$(ifstatus wan2 | jsonfilter -e '@.l3_device'), lan=$(ifstatus lan | jsonfilter -e '@.l3_device')"

# $1 = connections, $2 = family (4|6), $3 = optional source address; prints "N wanX" pairs
count() {
	if [ "$2" = 6 ]; then dst='TCP6:[2001:db8:ffff::10]:80'; else dst='TCP4:203.0.113.10:80'; fi
	if [ -n "$3" ]; then if [ "$2" = 6 ]; then dst="$dst,bind=[$3]"; else dst="$dst,bind=$3"; fi; fi
	nsx lan sh -c "for i in \$(seq $1); do socat -T5 - $dst,connect-timeout=5 </dev/null 2>/dev/null; done" | sort | uniq -c | tr '\n' ' '
}
n_of() { echo "$1" | sed -n "s/.*\b\([0-9][0-9]*\) $2\b.*/\1/p" | head -1; }
sum_of() { echo "$1" | awk '{s = 0; for (i = 1; i < NF; i += 2) if ($(i + 1) ~ /^wan[12]$/) s += $i} END {print s + 0}'; }
# count_all: as count, but retries the whole batch once if connections went unanswered; only for
# checks where every line is healthy (a failover check must see a lost connection, not retry it)
count_all() { c=$(count "$@"); [ "$(sum_of "$c")" -eq "$1" ] || c=$(count "$@"); echo "$c"; }
say "unmanaged baseline over IPv4 (main default route only): $(count 6 4)"


# --- 5. swanix on the real system
jget() { jsonfilter -e "$1"; }
wan_state() { # $1 wan name, $2 family (4|6) -> state
	ubus call swanix status 2>/dev/null | jsonfilter -e "@.wans[@.name='$1'].families[@.family=$2].state"
}
wan_share() {
	ubus call swanix status 2>/dev/null | jsonfilter -e "@.wans[@.name='$1'].families[@.family=$2].new_connection_share.balanced"
}
wait_for() { # description, timeout, command...
	d=$1; t=$2; shift 2; i=0
	while [ $i -lt $t ]; do "$@" && return 0; i=$((i+1)); sleep 1; done
	fail "timed out waiting for: $d"
}
both_healthy() { [ "$(wan_state wan1 $1)" = healthy ] && [ "$(wan_state wan2 $1)" = healthy ]; }

uci set swanix.global.lan_network='lan'
uci -q delete swanix.health_default.target
uci add_list swanix.health_default.target='203.0.113.10:80'
uci add_list swanix.health_default.target='[2001:db8:ffff::10]:80'
uci set swanix.health_default.interval=2; uci set swanix.health_default.timeout=2
uci set swanix.health_default.down_rounds=2; uci set swanix.health_default.up_rounds=2; uci set swanix.health_default.recover_ramp=6
uci set swanix.global.update_interval=2; uci set swanix.global.sample_interval=1
uci set swanix.ipv6.mode=unmanaged
uci commit swanix

swanix check >/tmp/check.out 2>&1 || { cat /tmp/check.out; fail "swanix check"; }
ok "swanix check: $(cat /tmp/check.out)"
swanix doctor >/tmp/doctor.out 2>&1; rc=$?
cat /tmp/doctor.out | sed 's/^/    doctor: /'
[ $rc -eq 0 ] || fail "swanix doctor reports a blocking problem on the real system"
ok "doctor passes against real fw4/netifd state"

# apply with the confirmation window, through the real procd init script
apply_confirm() {
	ubus call swanix prepare >/dev/null || fail "rpcd prepare"
	uci commit swanix
	swanix apply --timeout 120s --verify 120s >/tmp/apply.out 2>&1 || { cat /tmp/apply.out; logread | tail -20; fail "swanix apply"; }
	swanix confirm >/tmp/confirm.out 2>&1 || { cat /tmp/confirm.out; fail "swanix confirm"; }
}
uci set swanix.global.enabled=1; uci set swanix.global.startup_mode=manage
apply_confirm
ok "apply + confirm ($(cat /tmp/apply.out | tail -1))"
[ "$(/etc/init.d/swanix enabled && echo yes)" = yes ] || fail "service not enabled after confirm"
pidof swanix >/dev/null || fail "daemon not running under procd"
nft list tables | grep -q "inet swanix" || fail "nft table missing"
wait_for "both lines healthy (IPv4)" 60 both_healthy 4
ok "status through rpcd/ubus: wan1=$(wan_state wan1 4) wan2=$(wan_state wan2 4), shares $(wan_share wan1 4)/$(wan_share wan2 4)"

r=$(count_all 120 4); say "IPv4 distribution (120 connections): $r"
w1=$(n_of "$r" wan1); w2=$(n_of "$r" wan2)
[ $(( ${w1:-0} + ${w2:-0} )) -eq 120 ] || fail "lost connections: $r"
[ ${w1:-0} -ge 45 ] && [ ${w2:-0} -ge 20 ] || fail "distribution outside the expected 62/38 range: $r"
ok "IPv4 connections are balanced over both lines ($r)"

# --- 5b. the real LuCI (uhttpd + rpcd) is driven by a browser on the host (see run.py --host-step)
echo "VMTEST-HOST-STEP: luci"
read -t 900 hostanswer
if [ "$hostanswer" = HOSTSKIP ]; then
	say "no host browser available in this run; the LuCI browser flow is not exercised here"
else
	[ "$hostanswer" = HOSTDONE ] || fail "browser step against the real LuCI reported: $hostanswer"
	ok "real LuCI in a browser: every swanix page renders live data without errors, Save & Apply -> Confirm worked"
	[ "$(uci get swanix.health_default.interval)" = 7 ] || fail "the interval edited in the browser was not committed (got $(uci get swanix.health_default.interval))"
	[ ! -e /var/run/swanix/apply.json ] || fail "a pending switch is left after the browser confirmed"
	pidof swanix >/dev/null || fail "daemon not running after the browser apply"
	wait_for "both lines healthy after the browser apply" 60 both_healthy 4
	ok "browser apply committed the change (interval 7), confirmed it, the daemon runs with it"
	uci set swanix.health_default.interval=2; apply_confirm
fi

# --- 6. failure handling on the real system
all_via() { # $1 wan name, $2 connections, $3 family, $4 source
	# a connection on any other line is a hard failure; an unanswered connection is retried once as a whole batch
	for attempt in 1 2; do
		r=$(count $2 $3 $4); n=$(n_of "$r" $1)
		total=$(echo "$r" | awk '{s += $1} END {print s + 0}')
		[ "$total" -eq "${n:-0}" ] || { say "connections left through another line: $r"; return 1; }
		[ "${n:-0}" -eq "$2" ] && return 0
		say "attempt $attempt: only ${n:-0} of $2 connections answered: $r"
	done
	return 1
}
state_is() { [ "$(wan_state $1 $2)" = "$3" ]; }
both_carry() { # $1 connections: all answered (one retry if some went unanswered) and both lines used
	for attempt in 1 2; do
		r=$(count $1 4)
		[ "$(sum_of "$r")" -eq "$1" ] && [ -n "$(n_of "$r" wan1)" ] && [ -n "$(n_of "$r" wan2)" ] && return 0
		say "attempt $attempt: $r"
	done
	return 1
}

# 6a. the upstream of wan2 stops answering: probes notice, wan2 leaves the pool
stop_servers 2
wait_for "wan2 down after its upstream died" 40 state_is wan2 4 down
all_via wan1 30 4 || fail "traffic still reached the dead line"
ok "upstream failure: wan2 down, 30/30 new connections on wan1"
start_servers 2
wait_for "wan2 healthy again" 60 state_is wan2 4 healthy
ok "recovery: wan2 healthy again"

# 6b. netifd takes the interface down and up (real hotplug / netlink path)
ifdown wan2
wait_for "wan2 down after ifdown" 15 state_is wan2 4 down
all_via wan1 20 4 || fail "traffic reached the line that netifd took down"
ok "ifdown wan2: down within 15 s, all new connections on wan1"
ifup wan2
wait_for "wan2 healthy after ifup" 90 state_is wan2 4 healthy
ok "ifup wan2: healthy again"

# 6c. firewall reload must not disturb us, nor we it
/etc/init.d/firewall reload >/dev/null 2>&1; sleep 4
nft list tables | grep -q "inet swanix" || fail "firewall reload removed the swanix table"
nft list tables | grep -q "inet fw4" || fail "fw4 table missing after reload"
r=$(count_all 40 4); [ $(sum_of "$r") -eq 40 ] || fail "connectivity lost across firewall reload: $r"
ok "firewall reload: both tables intact, 40/40 connections ($r)"

# --- 7. IPv6
wait_for "both lines healthy (IPv6 probes need the ipv6 section enabled first)" 1 true
uci set swanix.ipv6.mode=nat66; uci -q delete swanix.ipv6.nat66_source; uci add_list swanix.ipv6.nat66_source='fd00:1::/64'
apply_confirm
wait_for "both lines healthy (IPv6)" 60 both_healthy 6
r=$(count_all 80 6); say "IPv6 NAT66 distribution (80 connections): $r"
w1=$(n_of "$r" wan1); w2=$(n_of "$r" wan2)
[ $(( ${w1:-0} + ${w2:-0} )) -eq 80 ] || fail "lost IPv6 connections: $r"
[ ${w1:-0} -ge 20 ] && [ ${w2:-0} -ge 10 ] || fail "NAT66 distribution outside the expected range: $r"
ok "nat66: IPv6 connections balanced over both lines through source translation ($r)"

# 7b. native IPv6: no translation, the source prefix decides the line. netifd hands each WAN a
# delegated /56 (static ip6prefix) and assigns a /64 of each to the LAN.
uci set swanix.ipv6.mode=native
uci add_list network.wan.ip6prefix='2001:db8:1::/56'; uci add_list network.wan2.ip6prefix='2001:db8:2::/56'
uci set network.lan.ip6assign=64
uci commit network
/etc/init.d/network reload >/dev/null 2>&1
has_prefixes() { ip -6 addr show dev br-lan | grep -q 'inet6 2001:db8:1:' && ip -6 addr show dev br-lan | grep -q 'inet6 2001:db8:2:'; }
wait_for "netifd assigned a /64 of each delegated prefix to the LAN" 60 has_prefixes
ifstatus lan | jsonfilter -e '@["ipv6-prefix-assignment"][*].address' | tr '\n' ' ' | sed 's/^/VMTEST: lan prefix assignments: /'; echo
nsx lan ip -6 addr add 2001:db8:1::100/64 dev vl1 nodad; nsx lan ip -6 addr add 2001:db8:2::100/64 dev vl1 nodad
wait_for "both lines healthy again after the network reload" 90 both_healthy 4
apply_confirm
wait_for "both lines healthy (IPv6)" 90 both_healthy 6
all_via wan1 20 6 2001:db8:1::100 || fail "native: source in the wan1 prefix did not leave through wan1"
all_via wan2 20 6 2001:db8:2::100 || fail "native: source in the wan2 prefix did not leave through wan2"
ok "native IPv6: sources in each line's prefix leave through that line (20/20 each), no translation"
# a line failing rejects its prefix instead of sending it out of the other line
ifdown wan2
wait_for "wan2 IPv6 down" 20 state_is wan2 6 down
r=$(count 6 6 2001:db8:2::100); [ -z "$(n_of "$r" wan1)" ] && [ -z "$(n_of "$r" wan2)" ] || fail "wan2-prefix traffic leaked while its line is down: $r"
all_via wan1 10 6 2001:db8:1::100 || fail "wan1-prefix traffic broke while wan2 was down"
ok "native IPv6: with wan2 down its prefix is rejected, not rerouted; wan1's prefix unaffected"
ifup wan2
wait_for "wan2 healthy after ifup (IPv6)" 90 state_is wan2 6 healthy

# --- 7c. dynamic uplinks: wan1 over DHCPv4 + DHCPv6 (address and prefix delegation, RAs), wan2 over PPPoE
# with IPv6 by DHCPv6-PD on the session. The ISP side: a second dnsmasq in isp1 for DHCPv4, an rp-pppoe
# access concentrator in isp2, and isp6 (ci/test/vm/isp6: RAs + DHCPv6, works on PPP) for IPv6 on both.
ISP6=$D/isp6
isp1_dhcp4() { # $1 = first host of the pool
	[ -f /tmp/isp1-dnsmasq.pid ] && kill $(cat /tmp/isp1-dnsmasq.pid) 2>/dev/null; rm -f /tmp/isp1.leases; sleep 1
	( exec ip netns exec isp1 dnsmasq -k --port=0 --conf-file=/dev/null --interface=vi1 --bind-interfaces \
		--dhcp-range=192.0.2.$1,192.0.2.$(($1 + 9)),255.255.255.0,2m --dhcp-option=3,192.0.2.1 --dhcp-authoritative \
		--dhcp-leasefile=/tmp/isp1.leases --pid-file=/tmp/isp1-dnsmasq.pid >/tmp/isp1-dnsmasq.log 2>&1 ) &
}
isp1_route6() { # the delegated prefix is routed to the router's link-local address on that line
	ll=$(ip -6 addr show dev vw1 scope link | sed -n 's/.*inet6 \(fe80[^/]*\).*/\1/p' | head -1)
	nsx isp1 ip -6 route replace $1 via $ll dev vi1
}
isp2_concentrator() { # $1 = first remote address, $2 = delegated /56 for the session
	[ -f /tmp/isp2-pppoe.pid ] && kill $(cat /tmp/isp2-pppoe.pid) 2>/dev/null
	cat > /tmp/isp2-ipv6-up <<EOF
#!/bin/sh
# runs in isp2 when a session's IPv6CP is up; \$1 = its ppp interface
ip -6 route replace $2 dev \$1
ip -6 route replace 2001:db8:bb::/64 dev \$1
[ -f /tmp/isp2-isp6.pid ] && kill \$(cat /tmp/isp2-isp6.pid) 2>/dev/null
$ISP6 -i \$1 -pool 2001:db8:bb::/64 -pd $2 >>/tmp/isp2-isp6.log 2>&1 &
echo \$! > /tmp/isp2-isp6.pid
EOF
	chmod +x /tmp/isp2-ipv6-up
	( exec ip netns exec isp2 pppoe-server -F -k -I vi2 -L 100.64.2.1 -R $1 -N 4 -O /tmp/isp2-ppp-options >/tmp/isp2-pppoe.log 2>&1 ) &
	echo $! > /tmp/isp2-pppoe.pid
}
printf 'require-pap\nlcp-echo-interval 2\nlcp-echo-failure 3\nmru 1492\nmtu 1492\n+ipv6\nnodefaultroute\nipv6-up-script /tmp/isp2-ipv6-up\n' > /tmp/isp2-ppp-options
echo 'swanix * swanixpw *' >> /etc/ppp/pap-secrets
for n in isp1 isp2; do nsx $n sysctl -qw net.ipv6.conf.all.forwarding=1; done
isp1_dhcp4 100
echo 2001:db8:1:100::/56 > /tmp/isp1.pd
( ISP6_PD_FILE=/tmp/isp1.pd exec ip netns exec isp1 $ISP6 -i vi1 -pool 2001:db8:a::/64 -pd 2001:db8:1:100::/56 >/tmp/isp1-isp6.log 2>&1 ) &
echo $! > /tmp/isp1-isp6.pid
isp2_concentrator 100.64.2.2 2001:db8:2:200::/56
for o in ipaddr gateway ip6addr ip6gw ip6prefix; do uci -q delete network.wan.$o; uci -q delete network.wan2.$o; done
uci set network.wan.proto=dhcp
uci set network.wan6=interface; uci set network.wan6.device=vw1; uci set network.wan6.proto=dhcpv6
uci set network.wan6.reqaddress=try; uci set network.wan6.reqprefix=56
uci set network.wan2.proto=pppoe; uci set network.wan2.username=swanix; uci set network.wan2.password=swanixpw; uci set network.wan2.ipv6=auto
uci commit network
uci set firewall.@zone[1].network='wan wan6 wan2'; uci commit firewall
/etc/init.d/network reload >/dev/null 2>&1; /etc/init.d/firewall reload >/dev/null 2>&1
v4of() { ifstatus $1 2>/dev/null | jsonfilter -e '@["ipv4-address"][0].address' 2>/dev/null; }
pdof() { ifstatus $1 2>/dev/null | jsonfilter -e '@["ipv6-prefix"][*].address' 2>/dev/null | grep -v '^::$' | head -1; }
uplinks_up() { [ -n "$(v4of wan)" ] && [ -n "$(v4of wan2)" ] && [ -n "$(pdof wan6)" ] && [ -n "$(pdof wan2_6)" ]; }
wait_for "DHCPv4, DHCPv6-PD and PPPoE (with PD) uplinks up" 90 uplinks_up
isp1_route6 2001:db8:1:100::/56
ok "netifd: wan DHCPv4 $(v4of wan), wan6 DHCPv6-PD $(pdof wan6)/56; wan2 PPPoE $(v4of wan2) on $(ifstatus wan2 | jsonfilter -e '@.l3_device'), wan2_6 PD $(pdof wan2_6)/56"
nsx lan ip -6 addr add 2001:db8:1:100::100/64 dev vl1 nodad; nsx lan ip -6 addr add 2001:db8:2:200::100/64 dev vl1 nodad
wait_for "both lines healthy over the dynamic uplinks (IPv4)" 90 both_healthy 4
wait_for "both lines healthy over the dynamic uplinks (IPv6)" 90 both_healthy 6
dist=$(count_all 60 4); w1=$(n_of "$dist" wan1); w2=$(n_of "$dist" wan2)
[ $(( ${w1:-0} + ${w2:-0} )) -eq 60 ] && [ ${w1:-0} -ge 15 ] && [ ${w2:-0} -ge 10 ] || fail "IPv4 over DHCP + PPPoE not balanced: $dist"
all_via wan1 20 6 2001:db8:1:100::100 || fail "native: the DHCPv6-PD prefix did not leave through wan1"
all_via wan2 20 6 2001:db8:2:200::100 || fail "native: the PPPoE PD prefix did not leave through wan2"
ok "dynamic uplinks: IPv4 balanced over DHCP + PPPoE ($dist); each delegated prefix leaves through its own line (20/20)"

# the ISP drops the PPPoE session and comes back with a new pool and a new delegated prefix
old2=$(v4of wan2)
isp2_concentrator 100.64.2.50 2001:db8:2:300::/56
for p in $(pidof pppd); do grep -q nic-vi2 /proc/$p/cmdline 2>/dev/null && kill $p; done
wait_for "wan2 down when its PPPoE session ends" 20 state_is wan2 4 down
all_via wan1 20 4 || fail "traffic reached wan2 while its PPPoE session was down"
newpd2() { [ "$(pdof wan2_6)" = 2001:db8:2:300:: ] && [ -n "$(v4of wan2)" ] && [ "$(v4of wan2)" != "$old2" ]; }
wait_for "PPPoE redial with a new address and prefix" 90 newpd2
wait_for "wan2 healthy after the redial" 90 state_is wan2 4 healthy
ip route show table 1002 | grep -q "dev pppoe-wan2 src $(v4of wan2)" || { ip route show table 1002; fail "wan2's table does not use the new session"; }
both_carry 60 || fail "after the redial not both lines carry all connections: $r"
nsx lan ip -6 addr add 2001:db8:2:300::100/64 dev vl1 nodad
wait_for "wan2 IPv6 healthy after the redial" 90 state_is wan2 6 healthy
all_via wan2 20 6 2001:db8:2:300::100 || fail "native: the new PPPoE prefix does not leave through wan2"
r=$(count 4 6 2001:db8:2:200::100); [ -z "$(n_of "$r" wan1)" ] && [ -z "$(n_of "$r" wan2)" ] || fail "the withdrawn prefix still gets out: $r"
ok "PPPoE redial: down at once, all on wan1 meanwhile; back with $(v4of wan2) (was $old2) and PD $(pdof wan2_6)/56, both carried; the old prefix no longer gets out"

# the ISP renumbers its DHCPv4 pool: the renewal is refused and the router takes a new lease
old1=$(v4of wan)
ct_old() { grep -c "dst=$old1 " /proc/net/nf_conntrack 2>/dev/null; }
ct_before=$(ct_old)
isp1_dhcp4 110
for p in $(pgrep -f 'udhcpc.*vw1'); do kill -USR1 $p; done
newv4() { [ -n "$(v4of wan)" ] && [ "$(v4of wan)" != "$old1" ] && ip route show table 1001 | grep -q "src $(v4of wan) "; }
wait_for "new DHCPv4 lease in wan1's table" 60 newv4
wait_for "wan1 healthy after the new lease" 60 state_is wan1 4 healthy
both_carry 60 || fail "after the new lease not both lines carry all connections: $r"
ok "DHCPv4 renumbering: $old1 -> $(v4of wan), wan1's table follows, both lines carry ($r)"
# swanix flushes no conntrack state itself: the kernel's masquerade drops the entries of an address that goes away
if [ -r /proc/net/nf_conntrack ] && [ "${ct_before:-0}" -gt 0 ]; then
	[ "$(ct_old)" = 0 ] || fail "$(ct_old) conntrack entries still translate to the old address $old1"
	ok "conntrack: the $ct_before entries masqueraded to $old1 were dropped by the kernel when the address went away"
else
	say "conntrack check skipped (no /proc/net/nf_conntrack or no entries for $old1 before the change)"
fi

# the ISP delegates a different prefix: odhcp6c renews, netifd re-assigns the LAN /64, swanix follows
echo 2001:db8:1:300::/56 > /tmp/isp1.pd; kill -HUP $(cat /tmp/isp1-isp6.pid); isp1_route6 2001:db8:1:300::/56
for p in $(pgrep -f 'odhcp6c.*vw1'); do kill -USR1 $p; done
newpd1() { ip -6 addr show dev br-lan | grep -q 'inet6 2001:db8:1:300::1/64'; }
wait_for "the LAN gets a /64 of the renumbered prefix" 60 newpd1
nsx lan ip -6 addr add 2001:db8:1:300::100/64 dev vl1 nodad
selects() { nft list chain inet swanix select_lan | grep -q '2001:db8:1:300::/64 jump sel_wan1'; }
wait_for "swanix selects by the renumbered prefix" 30 selects
all_via wan1 20 6 2001:db8:1:300::100 || fail "native: the renumbered DHCPv6-PD prefix does not leave through wan1"
ok "DHCPv6-PD renumbering: the new prefix leaves through wan1 (20/20)"

# --- 7d. software flow offloading (fw4 flow_offloading 1, declared as global.flow_offload software): established
# flows leave the slow path through the flowtable and keep the route of their first packets, i.e. swanix's choice
uci set firewall.@defaults[0].flow_offloading=1; uci commit firewall
/etc/init.d/firewall reload >/dev/null 2>&1
nft list flowtables 2>/dev/null | grep -q flowtable || fail "fw4 did not create its flowtable"
swanix doctor >/tmp/doctor.out 2>&1 && { cat /tmp/doctor.out; fail "doctor accepted flow offloading that global.flow_offload does not declare"; }
uci set swanix.global.flow_offload=software
apply_confirm
swanix doctor >/tmp/doctor.out 2>&1 || { cat /tmp/doctor.out; fail "doctor rejects declared software offloading"; }
wait_for "both lines healthy with offloading" 60 both_healthy 4
for n in 1 2; do
	( exec ip netns exec isp$n socat TCP4-LISTEN:81,bind=203.0.113.10,fork,reuseaddr SYSTEM:"echo wan$n; head -c 20000000 /dev/zero; sleep 3" >/dev/null 2>&1 ) &
	echo $! >> /tmp/bulk.pids
done
sleep 1
bulk() { # one 20 MB download; prints "<line> <bytes>"
	nsx lan sh -c 'socat -u TCP4:203.0.113.10:81,connect-timeout=3 - 2>/dev/null | { read b; echo "$b $(wc -c)"; }'
}
: > /tmp/bulk.out
pids=""
for i in 1 2 3 4 5 6 7 8 9 10 11 12; do bulk >> /tmp/bulk.out & pids="$pids $!"; done
sleep 2
offloaded=$(grep 'dport=81 ' /proc/net/nf_conntrack | grep -c 'OFFLOAD')
for p in $pids; do wait $p; done
say "bulk transfers with offloading: $(sort /tmp/bulk.out | uniq -c | tr '\n' ' ') ; $offloaded flows marked OFFLOAD in conntrack"
[ "$offloaded" -gt 0 ] || fail "no flow was offloaded"
[ "$(grep -c ' 20000000$' /tmp/bulk.out)" -eq 12 ] || fail "transfers incomplete or broken through the flowtable: $(cat /tmp/bulk.out)"
grep -q '^wan1 ' /tmp/bulk.out && grep -q '^wan2 ' /tmp/bulk.out || fail "offloaded transfers used only one line: $(cat /tmp/bulk.out)"
ok "software offloading: 12/12 transfers of 20 MB complete over both lines, $offloaded flows offloaded"
dist=$(count_all 60 4); w1=$(n_of "$dist" wan1); w2=$(n_of "$dist" wan2)
[ $(sum_of "$dist") -eq 60 ] && [ ${w1:-0} -ge 15 ] && [ ${w2:-0} -ge 10 ] || fail "distribution with offloading: $dist"
stop_servers 2
wait_for "wan2 down with offloading" 40 state_is wan2 4 down
all_via wan1 30 4 || fail "offloading: new connections still reach the dead line"
start_servers 2
wait_for "wan2 healthy again with offloading" 60 state_is wan2 4 healthy
ok "software offloading: new connections balanced ($dist), failover moves them, recovery"
for p in $(cat /tmp/bulk.pids); do kill $p 2>/dev/null; done
uci set firewall.@defaults[0].flow_offloading=0; uci commit firewall; /etc/init.d/firewall reload >/dev/null 2>&1
uci set swanix.global.flow_offload=off
apply_confirm

# --- 8. the confirmation window on the real system: no confirm, everything is restored
before_interval=$(uci get swanix.health_default.interval)
ubus call swanix prepare >/dev/null
uci set swanix.health_default.interval=4; uci commit swanix
swanix apply --timeout 15s --verify 120s >/tmp/apply2.out 2>&1 || { cat /tmp/apply2.out; fail "second apply"; }
# the 15 s window starts once the switch has been verified; the guard then rolls back
rolled_back() { ! nft list tables | grep -q "inet swanix" && [ ! -e /var/run/swanix/apply.json ]; }
wait_for "the guard rolls back after the window" 60 rolled_back
[ "$(uci get swanix.health_default.interval)" = "$before_interval" ] || fail "configuration was not restored (interval $(uci get swanix.health_default.interval), expected $before_interval)"
/etc/init.d/swanix running >/dev/null 2>&1 && fail "the service still runs after the rollback"
ip rule show | grep -q fwmark && fail "policy-routing rules left after the rollback"
swanix confirm >/dev/null 2>&1 && fail "a late confirm must fail"
r=$(count_all 6 4); [ $(sum_of "$r") -eq 6 ] || fail "no connectivity after the rollback: $r"
ok "no confirmation: table, rules and service withdrawn, configuration restored, connectivity intact"

# --- 9. in-place upgrade keeps the service, removal withdraws only what we installed
apply_confirm
nft list tables | grep -q "inet swanix" || fail "swanix not active before the upgrade"
if [ -f /etc/opkg/distfeeds.conf ]; then
	opkg install --force-reinstall $D/pkgs/swanix-lite_*.ipk >/tmp/upgrade.log 2>&1 || { cat /tmp/upgrade.log; fail "reinstall"; }
else
	apk fix --reinstall --allow-untrusted swanix-lite >/tmp/upgrade.log 2>&1 || apk add --allow-untrusted --force-overwrite $D/pkgs/swanix-lite-*.apk >/tmp/upgrade.log 2>&1 || { cat /tmp/upgrade.log; fail "reinstall"; }
fi
upgraded() { pidof swanix >/dev/null && nft list tables | grep -q "inet swanix"; }
wait_for "the service is running again after the in-place upgrade" 40 upgraded
wait_for "both lines healthy after the upgrade" 60 both_healthy 4
ok "in-place upgrade: service restarted, table re-applied, both lines healthy"
if [ -f /etc/opkg/distfeeds.conf ]; then opkg remove luci-i18n-swanix-zh-cn luci-app-swanix swanix-lite swanix-common >/tmp/remove.log 2>&1; else apk del luci-i18n-swanix-zh-cn luci-app-swanix swanix-lite swanix-common >/tmp/remove.log 2>&1; fi || { cat /tmp/remove.log; fail "package removal"; }
sleep 2
nft list tables | grep -q "inet swanix" && fail "the nft table survived package removal"
nft list tables | grep -q "inet fw4" || fail "fw4 table disappeared"
ip rule show | grep -q fwmark && fail "policy-routing rules survived package removal"
[ -e /usr/bin/swanix ] && fail "binary left behind"
pidof swanix >/dev/null && fail "daemon still running after removal"
r=$(count_all 6 4); [ $(sum_of "$r") -eq 6 ] || fail "no connectivity after removal: $r"
ok "package removal: swanix table, rules and daemon gone, fw4 untouched, connectivity intact"
echo "VMTEST-RESULT: PASS"
