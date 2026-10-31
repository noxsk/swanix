package kernel

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

const (
	preroutingPriority = -149 // right after fw4's mangle chains (-150)
	outputPriority     = -149
)

// Ruleset is a rendered nftables script plus anything the operator should know.
type Ruleset struct {
	Script   string
	Warnings []string
}

func hex(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func chainWAN(id int) string {
	if id == RejectID {
		return "sel_reject"
	}
	return "sel_wan" + strconv.Itoa(id)
}

func pmChain(id int) string {
	if id == RejectID {
		return "pm_reject"
	}
	return "pm_wan" + strconv.Itoa(id)
}

func setWANIf(id int) string { return "wan" + strconv.Itoa(id) + "_if" }

func policyMap(name string, fam int) string { return fmt.Sprintf("pol_%s_%d", name, fam) }

func affChain(name string, fam int) string { return fmt.Sprintf("aff_%s_%d", name, fam) }
func affSet(name string, id, fam int) string {
	return fmt.Sprintf("affset_%s_%d_%d", name, id, fam)
}

// stickyPolicy reports whether the policy keeps clients on a line across connections
// for this family. Native IPv6 never balances, so it never needs it.
func (in *Input) stickyPolicy(name string, fam int) (config.Policy, bool) {
	p, ok := in.Cfg.PolicyByName(name)
	if !ok || p.Affinity == config.AffinityConnection || !in.familyEnabled(fam) {
		return p, false
	}
	if fam == 6 && in.Cfg.IPv6.Mode == config.IPv6Native {
		return p, false
	}
	return p, true
}

// affKey returns the nft key expression and the matching set type.
func affKey(p config.Policy, fam int) (expr, typ string) {
	ip, t := "ip", "ipv4_addr"
	if fam == 6 {
		ip, t = "ip6", "ipv6_addr"
	}
	if p.Affinity == config.AffinitySourceDestination {
		return ip + " saddr . " + ip + " daddr", t + " . " + t
	}
	return ip + " saddr", t
}

// selectStmt is what a rule runs to place a new connection of a policy.
func (in *Input) selectStmt(policy string, fam int) string {
	if _, sticky := in.stickyPolicy(policy, fam); sticky {
		return "jump " + affChain(policy, fam)
	}
	return "numgen random mod 100 vmap @" + policyMap(policy, fam)
}

// affinityBody renders the rules of a sticky policy's chain: reuse the line a client
// already has (only lines that are in the pool right now), else pick by weight, and
// remember the choice. Every new connection refreshes the entry (update, not add), so
// it expires affinity_timeout after the client's last new connection.
func (in *Input) affinityBody(p config.Policy, fam int, lay Layout) []string {
	idm := hex(lay.IDMask())
	key, _ := affKey(p, fam)
	var ids []int
	for id, pct := range in.Shares[p.Name][fam] {
		if pct > 0 {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	var out []string
	for _, id := range ids {
		out = append(out, fmt.Sprintf("ct mark & %s == 0 %s @%s jump %s", idm, key, affSet(p.Name, id, fam), chainWAN(id)))
	}
	out = append(out, fmt.Sprintf("ct mark & %s == 0 numgen random mod 100 vmap @%s", idm, policyMap(p.Name, fam)))
	for _, id := range ids {
		out = append(out, fmt.Sprintf("ct mark & %s == %s update @%s { %s timeout %ds }", idm, hex(lay.WANMark(id)), affSet(p.Name, id, fam), key, p.AffinityTimeout))
	}
	return out
}

func quoteIfs(devs []string) string {
	var q []string
	seen := map[string]bool{}
	for _, d := range devs {
		if d != "" && !seen[d] {
			seen[d] = true
			q = append(q, strconv.Quote(d))
		}
	}
	sort.Strings(q)
	return strings.Join(q, ", ")
}

func prefixList(ps []netip.Prefix, fam int) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		if (fam == 4) != p.Addr().Is4() {
			continue
		}
		s := p.Masked().String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// weightElements renders the interval-map elements for one policy/family.
func (in *Input) weightElements(policy string, fam int) string {
	shares := in.Shares[policy][fam]
	var ids []int
	for id, pct := range shares {
		if pct > 0 {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	if len(ids) == 0 {
		return fmt.Sprintf("0-99 : jump %s", chainWAN(RejectID))
	}
	var parts []string
	lo := 0
	for i, id := range ids {
		hi := lo + shares[id] - 1
		if i == len(ids)-1 {
			hi = 99
		}
		parts = append(parts, fmt.Sprintf("%d-%d : jump %s", lo, hi, chainWAN(id)))
		lo = hi + 1
	}
	return strings.Join(parts, ", ")
}

// familyEnabled reports whether the family is managed at all.
func (in *Input) familyEnabled(fam int) bool {
	if fam == 6 && in.Cfg.IPv6.Mode == config.IPv6Unmanaged {
		return false
	}
	return true
}

func (in *Input) ruleExpr(r config.Rule, fam int) (string, bool) {
	var parts []string
	parts = append(parts, fmt.Sprintf("meta nfproto ipv%d", fam))
	ipkw := "ip"
	if fam == 6 {
		ipkw = "ip6"
	}
	for _, f := range []struct {
		kw   string
		list []string
	}{{"saddr", r.Src}, {"daddr", r.Dest}} {
		if len(f.list) == 0 {
			continue
		}
		var sel []string
		for _, s := range f.list {
			p, err := parsePrefixOrAddr(s)
			if err != nil {
				return "", false
			}
			if (fam == 4) == p.Addr().Is4() {
				sel = append(sel, s)
			}
		}
		if len(sel) == 0 {
			return "", false
		}
		parts = append(parts, fmt.Sprintf("%s %s { %s }", ipkw, f.kw, strings.Join(sel, ", ")))
	}
	switch r.Proto {
	case "icmp":
		if fam != 4 {
			return "", false
		}
		parts = append(parts, "meta l4proto icmp")
	case "icmpv6":
		if fam != 6 {
			return "", false
		}
		parts = append(parts, "meta l4proto icmpv6")
	case "tcp", "udp":
		parts = append(parts, "meta l4proto "+r.Proto)
		if len(r.DestPort) > 0 {
			parts = append(parts, fmt.Sprintf("th dport { %s }", strings.Join(r.DestPort, ", ")))
		}
	}
	return strings.Join(parts, " "), true
}

func parsePrefixOrAddr(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Render produces the complete table definition, replacing any previous
// instance of the swanix table in one atomic transaction.
func (in *Input) Render() (*Ruleset, error) {
	lay, err := NewLayout(in.Cfg.Kernel.MarkMask)
	if err != nil {
		return nil, err
	}
	rs := &Ruleset{}
	tbl := in.Cfg.Kernel.NFTTable
	ids := in.enabledIDs()
	idm := hex(lay.IDMask())
	loc := hex(lay.Local())
	clearSel := hex(^(lay.IDMask() | lay.Inbound()))
	clearAll := hex(lay.Keep())
	clearID := hex(^lay.IDMask())
	idInbLoc := hex(lay.IDMask() | lay.Inbound() | lay.Local())
	defPolicy := in.Cfg.EffectiveDefaultPolicy()

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("add table inet %s", tbl)
	w("delete table inet %s", tbl)
	w("table inet %s {", tbl)

	// sets
	w("\tset lan_if {\n\t\ttype ifname\n\t\telements = { %s }\n\t}", orEmptyIf(in.LAN.Devices))
	for _, id := range ids {
		w("\tset %s {\n\t\ttype ifname", setWANIf(id))
		if rt, ok := in.wan(id); ok {
			if el := quoteIfs([]string{rt.Device4, rt.Device6}); el != "" {
				w("\t\telements = { %s }", el)
			}
		}
		w("\t}")
	}
	byp4 := append([]string{"127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32"}, prefixList(in.LAN.Prefixes4, 4)...)
	byp6 := append([]string{"::1/128", "fe80::/10", "ff00::/8"}, prefixList(in.LAN.Prefixes6, 6)...)
	w("\tset bypass4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}", strings.Join(dedupe(byp4), ", "))
	w("\tset bypass6 {\n\t\ttype ipv6_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}", strings.Join(dedupe(byp6), ", "))
	if in.Cfg.IPv6.Mode == config.IPv6NAT66 {
		w("\tset nat66_src {\n\t\ttype ipv6_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}", strings.Join(in.Cfg.IPv6.NAT66Sources, ", "))
	}

	// counters: new connections assigned per WAN
	for _, id := range ids {
		w("\tcounter sel_wan%d {\n\t}", id)
	}
	w("\tcounter sel_reject {\n\t}")

	// selector and packet-mark chains, one pair per WAN
	for _, id := range append(append([]int{}, ids...), RejectID) {
		w("\tchain %s {", chainWAN(id))
		w("\t\tcounter name %q", chainWAN(id))
		w("\t\tct mark set ct mark & %s | %s", clearSel, hex(lay.WANMark(id)))
		w("\t\tmeta mark set meta mark & %s | %s", clearID, hex(lay.WANMark(id)))
		w("\t}")
		w("\tchain %s {", pmChain(id))
		w("\t\tmeta mark set meta mark & %s | %s", clearID, hex(lay.WANMark(id)))
		w("\t}")
	}

	// policy weight maps
	for _, p := range in.Cfg.Policies {
		for _, fam := range []int{4, 6} {
			if !in.familyEnabled(fam) || (fam == 6 && in.Cfg.IPv6.Mode == config.IPv6Native) {
				continue
			}
			w("\tmap %s {\n\t\ttypeof numgen random mod 100 : verdict\n\t\tflags interval\n\t\telements = { %s }\n\t}", policyMap(p.Name, fam), in.weightElements(p.Name, fam))
		}
	}

	// stickiness: per-line dynamic sets and one chain per sticky policy/family
	for _, p := range in.Cfg.Policies {
		for _, fam := range []int{4, 6} {
			if _, sticky := in.stickyPolicy(p.Name, fam); !sticky {
				continue
			}
			_, typ := affKey(p, fam)
			for _, m := range p.Members {
				if wn, ok := in.Cfg.WANByName(m); ok && wn.Enabled {
					w("\tset %s {\n\t\ttype %s\n\t\tflags dynamic,timeout\n\t\ttimeout %ds\n\t\tsize 65535\n\t}", affSet(p.Name, wn.ID, fam), typ, p.AffinityTimeout)
				}
			}
			w("\tchain %s {", affChain(p.Name, fam))
			for _, line := range in.affinityBody(p, fam, lay) {
				w("\t\t%s", line)
			}
			w("\t}")
		}
	}

	// packet marking, one lookup per packet: direction . (WAN ID | inbound bit) -> packet-mark
	// chain. Connections from the LAN or the router mark their original direction,
	// connections that arrived from a WAN mark only their replies; other keys miss.
	var pmEl []string
	for _, id := range ids {
		pmEl = append(pmEl, fmt.Sprintf("original . %s : jump %s", hex(lay.WANMark(id)), pmChain(id)),
			fmt.Sprintf("reply . %s : jump %s", hex(lay.WANMark(id)|lay.Inbound()), pmChain(id)))
	}
	pmEl = append(pmEl, fmt.Sprintf("original . %s : jump %s", hex(lay.WANMark(RejectID)), pmChain(RejectID)))
	w("\tmap pm_map {\n\t\ttypeof ct direction . ct mark : verdict\n\t\telements = { %s }\n\t}", strings.Join(pmEl, ", "))

	// selection chains
	w("\tchain select_lan {")
	in.renderSelection(&b, rs, false, idm, defPolicy)
	w("\t}")
	if in.Cfg.Global.ManageRouterTraffic {
		w("\tchain select_local {")
		in.renderSelection(&b, rs, true, idm, defPolicy)
		w("\t}")
	}

	pmRule := fmt.Sprintf("\t\tct direction . ct mark & %s vmap @pm_map", hex(lay.IDMask()|lay.Inbound()))

	// prerouting. Everything that decides a line runs once, on a connection's first
	// packet (prerouting_new); every later packet only copies the connection's line into
	// the packet mark with one map lookup.
	w("\tchain prerouting_new {")
	for _, id := range ids {
		w("\t\tiifname @%s ct mark set ct mark & %s | %s return", setWANIf(id), clearAll, hex(lay.WANMark(id)|lay.Inbound()))
	}
	w("\t\tfib daddr type local return")
	w("\t\tip daddr @bypass4 return")
	w("\t\tip6 daddr @bypass6 return")
	w("\t\tiifname @lan_if ct mark & %s == 0 jump select_lan", idm)
	w("\t}")
	w("\tchain prerouting {")
	w("\t\ttype filter hook prerouting priority %d; policy accept;", preroutingPriority)
	w("\t\tct state new jump prerouting_new")
	w(pmRule)
	w("\t}")

	// output: router-originated traffic. Without manage_router_traffic the router's own
	// connections follow main, but replies of inbound connections (services on the router,
	// ICMP errors about them) still leave through the line they arrived on.
	if in.Cfg.Global.ManageRouterTraffic {
		w("\tchain output_new {")
		w("\t\toifname \"lo\" return")
		w("\t\tip daddr @bypass4 return")
		w("\t\tip6 daddr @bypass6 return")
		w("\t\tct mark & %s == 0 ct mark set ct mark | %s", idm, loc)
		w("\t\tct mark & %s == 0 jump select_local", idm)
		w("\t}")
	}
	w("\tchain output {")
	w("\t\ttype route hook output priority %d; policy accept;", outputPriority)
	w("\t\tmeta mark & %s != 0 return", hex(lay.Probe()|lay.IDMask())) // probes and wan_policy sockets carry their line already
	if in.Cfg.Global.ManageRouterTraffic {
		w("\t\tct state new jump output_new")
	}
	w(pmRule)
	w("\t}")

	// NAT66 (only when explicitly selected)
	if in.Cfg.IPv6.Mode == config.IPv6NAT66 {
		w("\tchain nat66_postrouting {")
		w("\t\ttype nat hook postrouting priority srcnat; policy accept;")
		for _, id := range ids {
			w("\t\tmeta nfproto ipv6 oifname @%s ct mark & %s == %s ip6 saddr @nat66_src ip6 daddr != @bypass6 masquerade", setWANIf(id), idInbLoc, hex(lay.WANMark(id)))
			if in.Cfg.IPv6.NAT66Router {
				w("\t\tmeta nfproto ipv6 oifname @%s ct mark & %s == %s ip6 daddr != @bypass6 masquerade", setWANIf(id), idInbLoc, hex(lay.WANMark(id)|lay.Local()))
			}
		}
		w("\t}")
	}
	w("}")
	rs.Script = b.String()
	return rs, nil
}

func orEmptyIf(devs []string) string {
	q := quoteIfs(devs)
	if q == "" {
		return `""`
	}
	return q
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// renderSelection writes the body of select_lan / select_local.
func (in *Input) renderSelection(b *strings.Builder, rs *Ruleset, local bool, idm, defPolicy string) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	rules := append([]config.Rule(nil), in.Cfg.Rules...)
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })
	for _, fam := range []int{4, 6} {
		if !in.familyEnabled(fam) {
			continue
		}
		native6 := fam == 6 && in.Cfg.IPv6.Mode == config.IPv6Native
		if native6 {
			if local {
				// router-originated IPv6 follows the source address chosen by the kernel
				continue
			}
			for _, wid := range in.enabledIDs() {
				rt, ok := in.wan(wid)
				if !ok {
					continue
				}
				if ps := prefixList(rt.Prefixes6, 6); len(ps) > 0 {
					target := chainWAN(wid)
					if !rt.Up6 || rt.Down6 {
						target = chainWAN(RejectID)
					}
					w("\t\tct mark & %s == 0 ip6 saddr { %s } jump %s", idm, strings.Join(ps, ", "), target)
				}
			}
			continue
		}
		for _, r := range rules {
			if !r.Enabled || r.Local != local {
				continue
			}
			expr, ok := in.ruleExpr(r, fam)
			if !ok {
				continue
			}
			w("\t\tct mark & %s == 0 %s %s", idm, expr, in.selectStmt(r.Policy, fam))
		}
		if defPolicy != "" {
			w("\t\tct mark & %s == 0 meta nfproto ipv%d %s", idm, fam, in.selectStmt(defPolicy, fam))
		}
	}
	if native6Rules(in, local) {
		rs.Warnings = append(rs.Warnings, "ipv6.mode=native: IPv6 egress follows the source prefix; policy rules are not applied to IPv6")
	}
}

func native6Rules(in *Input, local bool) bool {
	if in.Cfg.IPv6.Mode != config.IPv6Native || local {
		return false
	}
	for _, r := range in.Cfg.Rules {
		if r.Enabled && !r.Local {
			return true
		}
	}
	return false
}

// RenderWeights refreshes only the weight maps. It is a single atomic batch
// and leaves chains, sets, counters and conntrack untouched.
func (in *Input) RenderWeights() string {
	tbl := in.Cfg.Kernel.NFTTable
	var b strings.Builder
	for _, p := range in.Cfg.Policies {
		for _, fam := range []int{4, 6} {
			if !in.familyEnabled(fam) || (fam == 6 && in.Cfg.IPv6.Mode == config.IPv6Native) {
				continue
			}
			m := policyMap(p.Name, fam)
			fmt.Fprintf(&b, "flush map inet %s %s\n", tbl, m)
			fmt.Fprintf(&b, "add element inet %s %s { %s }\n", tbl, m, in.weightElements(p.Name, fam))
			if _, sticky := in.stickyPolicy(p.Name, fam); sticky {
				// the chain lists the lines currently in the pool; stored client entries stay
				lay, _ := NewLayout(in.Cfg.Kernel.MarkMask)
				fmt.Fprintf(&b, "flush chain inet %s %s\n", tbl, affChain(p.Name, fam))
				for _, line := range in.affinityBody(p, fam, lay) {
					fmt.Fprintf(&b, "add rule inet %s %s %s\n", tbl, affChain(p.Name, fam), line)
				}
			}
		}
	}
	return b.String()
}

func (in *Input) setElements(b *strings.Builder, name string, elems string) {
	tbl := in.Cfg.Kernel.NFTTable
	fmt.Fprintf(b, "flush set inet %s %s\n", tbl, name)
	if elems != "" {
		fmt.Fprintf(b, "add element inet %s %s { %s }\n", tbl, name, elems)
	}
}

// RenderRuntime is the incremental update for interface, address and prefix
// changes: it refreshes sets, the selection chains and the weight maps in one
// atomic batch. Chains' hooks, counters, other chains and conntrack stay put.
func (in *Input) RenderRuntime() (*Ruleset, error) {
	lay, err := NewLayout(in.Cfg.Kernel.MarkMask)
	if err != nil {
		return nil, err
	}
	rs := &Ruleset{}
	tbl := in.Cfg.Kernel.NFTTable
	idm := hex(lay.IDMask())
	defPolicy := in.Cfg.EffectiveDefaultPolicy()
	var b strings.Builder
	in.setElements(&b, "lan_if", quoteIfs(in.LAN.Devices))
	for _, id := range in.enabledIDs() {
		var el string
		if rt, ok := in.wan(id); ok {
			el = quoteIfs([]string{rt.Device4, rt.Device6})
		}
		in.setElements(&b, setWANIf(id), el)
	}
	byp4 := append([]string{"127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32"}, prefixList(in.LAN.Prefixes4, 4)...)
	byp6 := append([]string{"::1/128", "fe80::/10", "ff00::/8"}, prefixList(in.LAN.Prefixes6, 6)...)
	in.setElements(&b, "bypass4", strings.Join(dedupe(byp4), ", "))
	in.setElements(&b, "bypass6", strings.Join(dedupe(byp6), ", "))
	chains := []struct {
		name  string
		local bool
	}{{"select_lan", false}}
	if in.Cfg.Global.ManageRouterTraffic {
		chains = append(chains, struct {
			name  string
			local bool
		}{"select_local", true})
	}
	for _, c := range chains {
		fmt.Fprintf(&b, "flush chain inet %s %s\n", tbl, c.name)
		var body strings.Builder
		in.renderSelection(&body, rs, c.local, idm, defPolicy)
		for _, line := range strings.Split(strings.TrimSpace(body.String()), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fmt.Fprintf(&b, "add rule inet %s %s %s\n", tbl, c.name, line)
			}
		}
	}
	b.WriteString(in.RenderWeights())
	rs.Script = b.String()
	return rs, nil
}
