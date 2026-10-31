package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
)

type fakeRunner map[string]string

func (f fakeRunner) Run(_ string, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	if v, ok := f[key]; ok {
		return v, nil
	}
	return "", errors.New("not stubbed: " + key)
}

type fakeNetif struct{}

func (fakeNetif) Status(name string) (netif.Interface, bool, error) {
	switch name {
	case "wan", "wan2":
		return netif.Interface{Name: name, Up: true, Available: true, Device: "eth-" + name}, true, nil
	case "lan":
		return netif.Interface{Name: "lan", Up: true, Available: true, Device: "br-lan"}, true, nil
	}
	return netif.Interface{}, false, nil
}

func env(t *testing.T, fw string, rules map[string]string) *Env {
	cfg, err := config.LoadFile("../../../openwrt/swanix-common/files/etc/config/swanix")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Global.Enabled = true
	r := fakeRunner{"nft list tables": "", "ip -V": "ip utility", "ubus list network.interface": "",
		"nft list ruleset": "", "ip -4 rule show": "0:\tfrom all lookup local\n32766:\tfrom all lookup main\n",
		"ip -6 rule show": "0:\tfrom all lookup local\n"}
	for k, v := range rules {
		r[k] = v
	}
	return &Env{Cfg: cfg, Runner: r, Netif: fakeNetif{}, ReadFile: func(p string) ([]byte, error) {
		if p == "/etc/config/firewall" {
			return []byte(fw), nil
		}
		return nil, errors.New("absent")
	}}
}

func find(cs []Check, prefix string, l Level) *Check {
	for i := range cs {
		if strings.HasPrefix(cs[i].Name, prefix) && cs[i].Level == l {
			return &cs[i]
		}
	}
	return nil
}

const goodFW = `
config defaults
	option flow_offloading '0'
config zone
	option name 'wan'
	list network 'wan'
	list network 'wan2'
	option masq '1'
`

func TestHealthySystemHasNoFailures(t *testing.T) {
	cs := Run(env(t, goodFW, nil))
	if HasFailure(cs) {
		t.Fatalf("unexpected failure: %+v", cs)
	}
	if find(cs, "fw4-masq:wan1", OK) == nil {
		t.Fatalf("masq coverage not confirmed: %+v", cs)
	}
}

func TestMasqMissing(t *testing.T) {
	fw := strings.Replace(goodFW, "option masq '1'", "option masq '0'", 1)
	if find(Run(env(t, fw, nil)), "fw4-masq:wan1", Fail) == nil {
		t.Fatal("missing masq must fail when router traffic is managed")
	}
}

func TestMasqSourceRestriction(t *testing.T) {
	fw := goodFW + "\tlist masq_src '192.168.1.0/24'\n"
	if find(Run(env(t, fw, nil)), "fw4-masq:wan1", Fail) == nil {
		t.Fatal("masq_src must be flagged")
	}
}

func TestFlowOffload(t *testing.T) {
	sw := strings.Replace(goodFW, "option flow_offloading '0'", "option flow_offloading '1'", 1)
	if find(Run(env(t, sw, nil)), "flow_offload", Fail) == nil {
		t.Fatal("undeclared software offload must block")
	}
	e := env(t, sw, nil)
	e.Cfg.Global.FlowOffload = config.OffloadSoftware
	if find(Run(e), "flow_offload", Fail) != nil || find(Run(e), "flow_offload", OK) == nil {
		t.Fatal("declared software offload must pass")
	}
	hw := strings.Replace(sw, "option flow_offloading '1'", "option flow_offloading '1'\n\toption flow_offloading_hw '1'", 1)
	e = env(t, hw, nil)
	e.Cfg.Global.FlowOffload = config.OffloadSoftware
	if find(Run(e), "flow_offload", Fail) == nil {
		t.Fatal("hardware offload must block")
	}
	e = env(t, goodFW, nil)
	e.Cfg.Global.FlowOffload = config.OffloadSoftware
	if find(Run(e), "flow_offload", Warn) == nil {
		t.Fatal("a declared but disabled offload must warn")
	}
}

func TestMarkOverlap(t *testing.T) {
	rules := map[string]string{"nft list ruleset": "table inet other {\n chain c {\n  meta mark set 0x01000000\n  meta mark set meta mark & 0xffffff00 | 0x2\n }\n}\n"}
	if find(Run(env(t, goodFW, rules)), "mark-mask", Warn) == nil {
		t.Fatal("overlapping mark not detected")
	}
	rules = map[string]string{"nft list ruleset": "table inet other {\n chain c {\n  meta mark set meta mark & 0xffffff00 | 0x2\n }\n}\n"}
	if find(Run(env(t, goodFW, rules)), "mark-mask", Warn) != nil {
		t.Fatal("non-overlapping mark flagged")
	}
}

func TestPriorityRangeOccupied(t *testing.T) {
	rules := map[string]string{"ip -4 rule show": "10050:\tfrom all fwmark 0x1 lookup 77\n32766:\tfrom all lookup main\n"}
	if find(Run(env(t, goodFW, rules)), "ip-rule", Fail) == nil {
		t.Fatal("occupied priority range must fail")
	}
}

func TestMwan3Detected(t *testing.T) {
	rules := map[string]string{"nft list ruleset": "table inet mwan3 {\n}\n"}
	if find(Run(env(t, goodFW, rules)), "mwan3", Warn) == nil {
		t.Fatal("mwan3 not detected")
	}
}
