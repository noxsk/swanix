package kernel

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

// largeInput is the biggest configuration the layout allows: 14 lines, 50 rules and a
// sticky policy, both families (NAT66).
func largeInput(b *testing.B) *Input {
	cfg, err := config.LoadFile(sampleConfig)
	if err != nil {
		b.Fatal(err)
	}
	cfg.Global.Enabled, cfg.Global.StartupMode = true, config.StartupManage
	cfg.IPv6.Mode, cfg.IPv6.NAT66Sources = config.IPv6NAT66, []string{"fd00:1::/64"}
	in := &Input{Cfg: cfg, LAN: LANRuntime{Devices: []string{"br-lan"},
		Prefixes4: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}, Prefixes6: []netip.Prefix{netip.MustParsePrefix("fd00:1::/64")}},
		Shares: Shares{}}
	cfg.WANs, cfg.Policies[0].Members = nil, nil
	sh4, sh6 := map[int]int{}, map[int]int{}
	for id := 1; id <= 14; id++ {
		name := fmt.Sprintf("wan%d", id)
		cfg.WANs = append(cfg.WANs, config.WAN{Name: name, ID: id, Enabled: true, Network: name, IPv6Network: "auto",
			DownloadMbps: 100, UploadMbps: 10, Preference: 100, Role: config.RoleActive, TrafficBias: config.BiasBalanced})
		cfg.Policies[0].Members = append(cfg.Policies[0].Members, name)
		in.WANs = append(in.WANs, WANRuntime{ID: id, Name: name, Device4: fmt.Sprintf("eth%d", id), Device6: fmt.Sprintf("eth%d", id),
			Gateway4: netip.MustParseAddr(fmt.Sprintf("10.%d.0.1", id)), Src4: netip.MustParseAddr(fmt.Sprintf("10.%d.0.2", id)), Up4: true,
			Gateway6: netip.MustParseAddr("fe80::1"), Up6: true})
		sh4[id], sh6[id] = 7, 7
	}
	sh4[1], sh6[1] = 9, 9
	cfg.Policies[0].Affinity, cfg.Policies[0].AffinityTimeout = config.AffinitySourceDestination, 300
	for i := 0; i < 50; i++ {
		cfg.Rules = append(cfg.Rules, config.Rule{Name: fmt.Sprintf("r%d", i), Enabled: true, Priority: 10 + i,
			Dest: []string{fmt.Sprintf("198.18.%d.0/24", i)}, Proto: "tcp", DestPort: []string{"443"}, Policy: "balanced"})
	}
	in.Shares["balanced"] = map[int]map[int]int{4: sh4, 6: sh6}
	if err := cfg.Validate(config.ProfileLite); err != nil {
		b.Fatal(err)
	}
	return in
}

// BenchmarkRender: the complete nft script for a full apply.
func BenchmarkRender(b *testing.B) {
	in := largeInput(b)
	rs, _ := in.Render()
	b.ReportMetric(float64(len(rs.Script)), "script_bytes")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := in.Render(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRenderWeights: the batch sent on every share change.
func BenchmarkRenderWeights(b *testing.B) {
	in := largeInput(b)
	b.ReportMetric(float64(len(in.RenderWeights())), "script_bytes")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = in.RenderWeights()
	}
}

// BenchmarkBuildRoutePlan: rules and routes for 14 lines, both families.
func BenchmarkBuildRoutePlan(b *testing.B) {
	in := largeInput(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := in.BuildRoutePlan(); err != nil {
			b.Fatal(err)
		}
	}
}
