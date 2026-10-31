package kernel

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

const sampleConfig = "../../../openwrt/swanix-common/files/etc/config/swanix"

func fixture(t *testing.T) *Input {
	t.Helper()
	cfg, err := config.LoadFile(sampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Global.Enabled = true
	cfg.Global.StartupMode = config.StartupManage
	if err := cfg.Validate(config.ProfileLite); err != nil {
		t.Fatal(err)
	}
	return &Input{
		Cfg: cfg,
		WANs: []WANRuntime{
			{ID: 1, Name: "wan1", Device4: "eth1", Gateway4: netip.MustParseAddr("192.0.2.1"), Src4: netip.MustParseAddr("192.0.2.2"), Up4: true,
				Device6: "eth1", Gateway6: netip.MustParseAddr("fe80::1"), Prefixes6: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")}, Up6: true},
			{ID: 2, Name: "wan2", Device4: "eth2", Gateway4: netip.MustParseAddr("198.51.100.1"), Src4: netip.MustParseAddr("198.51.100.2"), Up4: true,
				Device6: "eth2", Gateway6: netip.MustParseAddr("fe80::2"), Prefixes6: []netip.Prefix{netip.MustParsePrefix("2001:db8:2::/64")}, Up6: true},
		},
		LAN: LANRuntime{
			Devices:   []string{"br-lan"},
			Prefixes4: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")},
			Prefixes6: []netip.Prefix{netip.MustParsePrefix("fd00:1::/64")},
		},
		Shares: Shares{"balanced": {4: {1: 60, 2: 40}, 6: {1: 50, 2: 50}}},
	}
}

func needNetns(t *testing.T) { nstest.Require(t) }

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
