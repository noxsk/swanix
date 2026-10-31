package netif

import (
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

const wanJSON = `{"up":true,"available":true,"l3_device":"eth1","proto":"dhcp",
"ipv4-address":[{"address":"192.0.2.2","mask":24}],
"route":[{"target":"0.0.0.0","mask":0,"nexthop":"192.0.2.1","source":"192.0.2.2/32"},{"target":"192.0.2.0","mask":24,"nexthop":"0.0.0.0"}]}`

const wan6JSON = `{"up":true,"available":true,"l3_device":"eth1",
"ipv6-address":[{"address":"2001:db8:ffff::2","mask":64}],
"ipv6-prefix":[{"address":"2001:db8:1::","mask":56,"assigned":{"lan":{"address":"2001:db8:1::","mask":64}}}],
"route":[{"target":"::","mask":0,"nexthop":"fe80::1"}]}`

const lanJSON = `{"up":true,"available":true,"l3_device":"br-lan",
"ipv4-address":[{"address":"192.168.1.1","mask":24}],
"ipv6-address":[{"address":"fd00:1::1","mask":64}],
"ipv6-prefix-assignment":[{"address":"2001:db8:1::","mask":64}]}`

type fake map[string]string

func (f fake) Status(name string) (Interface, bool, error) {
	j, ok := f[name]
	if !ok {
		return Interface{}, false, nil
	}
	i, err := ParseStatus(name, []byte(j))
	return i, true, err
}

func TestCollect(t *testing.T) {
	cfg, err := config.LoadFile("../../../openwrt/swanix-common/files/etc/config/swanix")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Collect(cfg, fake{"wan": wanJSON, "wan6": wan6JSON, "lan": lanJSON})
	if err != nil {
		t.Fatal(err)
	}
	w1 := snap.WANs[0]
	if w1.Device4 != "eth1" || w1.Gateway4.String() != "192.0.2.1" || !w1.Up4 || w1.Src4.String() != "192.0.2.2" {
		t.Fatalf("wan1 v4: %+v", w1)
	}
	if !w1.Up6 || w1.Gateway6.String() != "fe80::1" || len(w1.Prefixes6) != 1 || w1.Prefixes6[0].String() != "2001:db8:1::/64" {
		t.Fatalf("wan1 v6 (auto via wan6): %+v", w1)
	}
	if len(snap.LAN.Devices) != 1 || snap.LAN.Devices[0] != "br-lan" || len(snap.LAN.Prefixes6) != 2 {
		t.Fatalf("lan: %+v", snap.LAN)
	}
	// wan2's interface does not exist: reported, not guessed
	if len(snap.Problems) == 0 || len(snap.WANs) != 2 || snap.WANs[1].Up4 {
		t.Fatalf("missing interface handling: %+v %v", snap.WANs[1], snap.Problems)
	}
}

func TestIPv6AutoAmbiguousIsNotGuessed(t *testing.T) {
	cfg, _ := config.LoadFile("../../../openwrt/swanix-common/files/etc/config/swanix")
	both := `{"up":true,"available":true,"l3_device":"eth1","ipv4-address":[{"address":"192.0.2.2","mask":24}],
"route":[{"target":"0.0.0.0","mask":0,"nexthop":"192.0.2.1"},{"target":"::","mask":0,"nexthop":"fe80::9"}]}`
	snap, _ := Collect(cfg, fake{"wan": both, "wan6": wan6JSON, "lan": lanJSON})
	if snap.WANs[0].Up6 {
		t.Fatal("ambiguous auto must not resolve")
	}
	found := false
	for _, p := range snap.Problems {
		if len(p) > 0 && p[:len("wan.wan1.ipv6_network")] == "wan.wan1.ipv6_network" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a problem report, got %v", snap.Problems)
	}
}

// PPPoE as netifd reports it: a point-to-point /32 whose default route points at the
// peer, and IPv6 on the dynamic "<network>_6" interface (DHCPv6-PD over pppoe-wan2).
const pppJSON = `{"up":true,"available":true,"l3_device":"pppoe-wan2","device":"vw2","proto":"pppoe",
"ipv4-address":[{"address":"100.64.2.2","mask":32,"ptpaddress":"100.64.2.1"}],
"route":[{"target":"0.0.0.0","mask":0,"nexthop":"100.64.2.1","source":"0.0.0.0/0"}]}`

const ppp6JSON = `{"up":true,"available":true,"l3_device":"pppoe-wan2","proto":"dhcpv6",
"ipv6-address":[{"address":"2001:db8:bb::105","mask":128}],
"ipv6-prefix":[{"address":"2001:db8:2:100::","mask":56,"assigned":{"lan":{"address":"2001:db8:2:100::","mask":64}}}],
"route":[{"target":"::","mask":0,"nexthop":"fe80::1","source":"::/0"}]}`

func TestCollectPPPoE(t *testing.T) {
	cfg, err := config.LoadFile("../../../openwrt/swanix-common/files/etc/config/swanix")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Collect(cfg, fake{"wan": wanJSON, "wan6": wan6JSON, "wan2": pppJSON, "wan2_6": ppp6JSON, "lan": lanJSON})
	if err != nil {
		t.Fatal(err)
	}
	w2 := snap.WANs[1]
	if w2.Device4 != "pppoe-wan2" || !w2.Up4 || w2.Gateway4.String() != "100.64.2.1" || w2.Src4.String() != "100.64.2.2" {
		t.Fatalf("pppoe v4: %+v", w2)
	}
	if w2.Device6 != "pppoe-wan2" || !w2.Up6 || len(w2.Prefixes6) != 1 || w2.Prefixes6[0].String() != "2001:db8:2:100::/64" {
		t.Fatalf("pppoe v6 (auto via wan2_6): %+v", w2)
	}
	if len(snap.Problems) != 0 {
		t.Fatalf("problems: %v", snap.Problems)
	}
}
