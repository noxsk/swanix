package config

import (
	"os"
	"strings"
	"testing"
)

const samplePath = "../../../openwrt/swanix-common/files/etc/config/swanix"

func TestSampleParsesAndValidates(t *testing.T) {
	cfg, err := LoadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(ProfileLite); err != nil {
		t.Fatalf("shipped default must validate: %v", err)
	}
	if len(cfg.WANs) != 2 || cfg.WANs[1].ID != 2 || cfg.Global.Enabled {
		t.Fatalf("unexpected decode: %+v", cfg.WANs)
	}
	if len(cfg.WANs[0].Name) == 0 || cfg.HealthFor(cfg.WANs[0]).Interval != 5 {
		t.Fatal("health reference not resolved")
	}
}

func mutate(t *testing.T, from, to string) error {
	t.Helper()
	data, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, from) {
		t.Fatalf("fixture lacks %q", from)
	}
	cfg, err := Parse(strings.Replace(s, from, to, 1))
	if err != nil {
		return err
	}
	return cfg.Validate(ProfileLite)
}

func TestRejections(t *testing.T) {
	cases := []struct{ name, from, to, want string }{
		{"duplicate id", "option id '2'", "option id '1'", "wan.wan2.id"},
		{"missing network", "option network 'wan2'", "option network 'wan'", "重复接口归属"},
		{"zero upload", "option upload_mbps '30'", "option upload_mbps '0'", "wan.wan2.upload_mbps"},
		{"unknown field", "option preference '100'\n\toption role 'active'\n\toption traffic_bias 'balanced'\n\toption health 'health_default'\n\nconfig health", "option bogus '1'\n\nconfig health", "未知字段"},
		{"bad policy member", "list member 'wan2'", "list member 'wan9'", "policy.balanced.member"},
		{"dup member", "list member 'wan2'", "list member 'wan1'", "重复"},
		{"proxy on lite", "config proxy 'proxy'\n\toption enabled '0'", "config proxy 'proxy'\n\toption enabled '1'", "proxy.enabled: 当前 lite 构建不含代理模块"},
		{"hardware offload", "option flow_offload 'off'", "option flow_offload 'hardware'", "global.flow_offload"},
		{"mask", "0xff000000", "0xf0f00000", "kernel.mark_mask"},
		{"default policy", "option default_policy 'balanced'", "option default_policy 'nope'", "global.default_policy"},
		{"dup section name", "config policy 'balanced'", "config policy 'wan1'", "唯一"},
		{"transparent tproxy not supported", "config proxy 'proxy'\n\toption enabled '0'\n\toption transparent_mode 'off'", "config proxy 'proxy'\n\toption enabled '0'\n\toption transparent_mode 'tproxy'", "proxy.transparent_mode"},
		{"bad affinity", "option affinity 'connection'", "option affinity 'sticky'", "policy.balanced.affinity"},
		{"saturation threshold out of range", "option unavailable_action 'reject'", "option saturation_threshold '40'\n\toption unavailable_action 'reject'", "policy.balanced.saturation_threshold"},
		{"bad bool", "option enabled '0'\n\toption startup_mode", "option enabled 'maybe'\n\toption startup_mode", "global.enabled"},
	}
	for _, c := range cases {
		err := mutate(t, c.from, c.to)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
}

func TestEnabledNeedsLAN(t *testing.T) {
	cfg, _ := Parse("config global 'global'\n\toption enabled '1'\n")
	err := cfg.Validate(ProfileLite)
	if err == nil || !strings.Contains(err.Error(), "global.lan_network") {
		t.Fatalf("got %v", err)
	}
}

func TestAnonymousSectionRejected(t *testing.T) {
	_, err := Parse("config wan\n\toption id '1'\n")
	if err == nil || !strings.Contains(err.Error(), "必须具名") {
		t.Fatalf("got %v", err)
	}
}

func TestMarkMask(t *testing.T) {
	m, s, err := ParseMarkMask("0xff000000")
	if err != nil || m != 0xff000000 || s != 24 {
		t.Fatal(m, s, err)
	}
	if _, _, err := ParseMarkMask("0xff00ff00"); err == nil {
		t.Fatal("non-contiguous must fail")
	}
}
