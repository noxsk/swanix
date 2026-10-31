package kernel

import (
	"strings"
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

func TestRenderContainsExpectedStructure(t *testing.T) {
	in := fixture(t)
	rs, err := in.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"add table inet swanix",
		"set wan1_if",
		`elements = { "eth1" }`,
		"map pol_balanced_4",
		"0-59 : jump sel_wan1, 60-99 : jump sel_wan2",
		// decisions on a connection's first packet only
		"ct state new jump prerouting_new",
		"iifname @wan1_if ct mark set ct mark & 0x00ffffff | 0x11000000 return",
		"fib daddr type local return",
		"iifname @lan_if ct mark & 0x0f000000 == 0 jump select_lan",
		// every packet: one lookup of direction . (line | inbound)
		"ct direction . ct mark & 0x1f000000 vmap @pm_map",
		"original . 0x01000000 : jump pm_wan1, reply . 0x11000000 : jump pm_wan1",
		"original . 0x0f000000 : jump pm_reject",
		"type route hook output",
		"meta mark & 0x4f000000 != 0 return",
		"ct state new jump output_new",
	} {
		if !strings.Contains(rs.Script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}

func TestRenderRejectWhenNoCandidates(t *testing.T) {
	in := fixture(t)
	in.Shares = Shares{"balanced": {4: {}, 6: {}}}
	rs, _ := in.Render()
	if !strings.Contains(rs.Script, "0-99 : jump sel_reject") {
		t.Fatal("empty candidate pool must reject, not fall back to main")
	}
}

func TestRouterTrafficOff(t *testing.T) {
	in := fixture(t)
	in.Cfg.Global.ManageRouterTraffic = false
	rs, _ := in.Render()
	if strings.Contains(rs.Script, "select_local") || strings.Contains(rs.Script, "ct state new") && strings.Contains(rs.Script, "| 0x20000000") {
		t.Fatal("router-originated connections must not be selected when router traffic is not managed")
	}
	// replies of inbound connections to the router still leave through the arrival line
	if !strings.Contains(rs.Script, "hook output") || !strings.Contains(rs.Script, "ct direction . ct mark & 0x1f000000 vmap @pm_map") || strings.Contains(rs.Script, "output_new") {
		t.Fatalf("output hook must keep inbound replies on their line:\n%s", rs.Script)
	}
}

func TestIPv6Unmanaged(t *testing.T) {
	in := fixture(t)
	in.Cfg.IPv6.Mode = config.IPv6Unmanaged
	rs, _ := in.Render()
	if strings.Contains(rs.Script, "pol_balanced_6") {
		t.Fatal("unmanaged IPv6 must not get a selector")
	}
}

func TestNativeIPv6SelectsBySourcePrefix(t *testing.T) {
	in := fixture(t)
	rs, _ := in.Render()
	if !strings.Contains(rs.Script, "ip6 saddr { 2001:db8:1::/64 } jump sel_wan1") {
		t.Fatalf("native v6 must select by source prefix:\n%s", rs.Script)
	}
	if strings.Contains(rs.Script, "map pol_balanced_6") {
		t.Fatal("native mode must not randomise IPv6 egress")
	}
}

func TestNAT66(t *testing.T) {
	in := fixture(t)
	in.Cfg.IPv6.Mode = config.IPv6NAT66
	in.Cfg.IPv6.NAT66Sources = []string{"fd00:1::/64"}
	in.Cfg.IPv6.NAT66Router = true
	rs, _ := in.Render()
	for _, want := range []string{"map pol_balanced_6", "set nat66_src", "type nat hook postrouting", "oifname @wan1_if ct mark & 0x3f000000 == 0x01000000 ip6 saddr @nat66_src", "== 0x21000000 ip6 daddr != @bypass6 masquerade"} {
		if !strings.Contains(rs.Script, want) {
			t.Errorf("lacks %q", want)
		}
	}
}

func TestRulesOrderedAndFamilySplit(t *testing.T) {
	in := fixture(t)
	in.Cfg.Policies = append(in.Cfg.Policies, config.Policy{Name: "only1", Members: []string{"wan1"}, Affinity: config.AffinityConnection, AffinityTimeout: 300, UnavailableAction: config.UnavailableReject})
	in.Cfg.Rules = []config.Rule{
		{Name: "r2", Enabled: true, Priority: 200, Policy: "only1", Proto: "tcp", DestPort: []string{"443", "8000-8100"}},
		{Name: "r1", Enabled: true, Priority: 100, Policy: "only1", Dest: []string{"203.0.113.0/24"}},
	}
	in.Shares["only1"] = map[int]map[int]int{4: {1: 100}, 6: {1: 100}}
	rs, _ := in.Render()
	i1 := strings.Index(rs.Script, "ip daddr { 203.0.113.0/24 }")
	i2 := strings.Index(rs.Script, "th dport { 443, 8000-8100 }")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("rules not ordered by priority:\n%s", rs.Script)
	}
	if strings.Contains(rs.Script, "ip6 daddr { 203.0.113.0/24 }") {
		t.Fatal("v4 destination leaked into v6 rule")
	}
}

func TestWeightsBatchTouchesOnlyMaps(t *testing.T) {
	in := fixture(t)
	s := in.RenderWeights()
	if !strings.Contains(s, "flush map inet swanix pol_balanced_4") || strings.Contains(s, "delete") || strings.Contains(s, "chain") {
		t.Fatalf("unexpected batch:\n%s", s)
	}
}

func TestLoadsInKernel(t *testing.T) {
	needNetns(t)
	in := fixture(t)
	rs, err := in.Render()
	if err != nil {
		t.Fatal(err)
	}
	f := t.TempDir() + "/swanix.nft"
	if err := writeFile(f, rs.Script); err != nil {
		t.Fatal(err)
	}
	sh(t, "nft", "-f", f)
	// idempotent full replace
	sh(t, "nft", "-f", f)
	// weight refresh is atomic and keeps counters/chains
	in.Shares["balanced"][4] = map[int]int{1: 10, 2: 90}
	wf := t.TempDir() + "/w.nft"
	if err := writeFile(wf, in.RenderWeights()); err != nil {
		t.Fatal(err)
	}
	sh(t, "nft", "-f", wf)
	out := sh(t, "nft", "list", "map", "inet", "swanix", "pol_balanced_4")
	if !strings.Contains(out, "0-9") || !strings.Contains(out, "10-99") {
		t.Fatalf("weights not updated:\n%s", out)
	}
}

func TestStickyPolicyRendering(t *testing.T) {
	in := fixture(t)
	in.Cfg.IPv6.Mode = config.IPv6Unmanaged
	in.Cfg.Policies[0].Affinity = config.AffinitySourceDestination
	in.Cfg.Policies[0].AffinityTimeout = 120
	in.Shares = Shares{"balanced": {4: {1: 60, 2: 40}}}
	rs, err := in.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set affset_balanced_1_4",
		"type ipv4_addr . ipv4_addr",
		"flags dynamic,timeout",
		"chain aff_balanced_4",
		"ip saddr . ip daddr @affset_balanced_1_4 jump sel_wan1",
		"update @affset_balanced_2_4 { ip saddr . ip daddr timeout 120s }",
		"jump aff_balanced_4",
	} {
		if !strings.Contains(rs.Script, want) {
			t.Errorf("lacks %q", want)
		}
	}
	// a line that left the pool is neither matched nor refreshed
	in.Shares["balanced"][4] = map[int]int{2: 100}
	w := in.RenderWeights()
	if strings.Contains(w, "affset_balanced_1_4") || !strings.Contains(w, "flush chain inet swanix aff_balanced_4") {
		t.Fatalf("weights batch must rebuild the sticky chain from the current pool:\n%s", w)
	}
}

func TestStickyLoadsInKernel(t *testing.T) {
	needNetns(t)
	for _, aff := range []string{config.AffinitySource, config.AffinitySourceDestination} {
		in := fixture(t)
		in.Cfg.IPv6.Mode = config.IPv6NAT66
		in.Cfg.IPv6.NAT66Sources = []string{"fd00:1::/64"}
		in.Cfg.Policies[0].Affinity = aff
		rs, err := in.Render()
		if err != nil {
			t.Fatal(err)
		}
		f := t.TempDir() + "/s.nft"
		if err := writeFile(f, rs.Script); err != nil {
			t.Fatal(err)
		}
		sh(t, "nft", "-f", f)
		in.Shares["balanced"][4] = map[int]int{2: 100}
		wf := t.TempDir() + "/w.nft"
		if err := writeFile(wf, in.RenderWeights()); err != nil {
			t.Fatal(err)
		}
		sh(t, "nft", "-f", wf)
	}
}
