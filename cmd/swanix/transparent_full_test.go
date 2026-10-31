//go:build !mw_lite

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

func TestSharedResourceChecks(t *testing.T) {
	const tunIn = `{"type": "tun", "tag": "tun-in", "address": ["172.19.0.1/30"], "auto_route": true%s}`
	const direct = `{"type": "direct", "tag": "wan"%s}`
	cases := []struct {
		name, mode, inbound, outbound, route, want string
	}{
		{"tun accepted", "tun", tunIn, `, "wan_policy": "balanced"`, "", ""},
		{"tun needs transparent_mode", "off", tunIn, `, "wan_policy": "balanced"`, "", "proxy.transparent_mode tun"},
		{"transparent_mode needs a tun", "tun", "", `, "wan_policy": "balanced"`, "", "没有启用 auto_route 的 tun 入站"},
		{"tun without wan_policy", "tun", tunIn, "", "", "没有出站设置 wan_policy"},
		{"tun rules after swanix's", "tun", strings.Replace(tunIn, "%s", `, "iproute2_rule_index": 10050%s`, 1), `, "wan_policy": "balanced"`, "", "iproute2_rule_index"},
		{"tun table inside swanix's", "tun", strings.Replace(tunIn, "%s", `, "iproute2_table_index": 1005%s`, 1), `, "wan_policy": "balanced"`, "", "iproute2_table_index"},
		{"auto_redirect", "tun", strings.Replace(tunIn, "%s", `, "auto_redirect": true%s`, 1), `, "wan_policy": "balanced"`, "", "auto_redirect"},
		{"default_mark in the window", "off", "", "", `, "route": {"final": "wan", "default_mark": 16777216}`, "route.default_mark"},
		{"routing_mark in the window", "off", "", `, "routing_mark": "0x2000000"`, "", "routing_mark"},
		{"default marks outside the window", "off", "", `, "routing_mark": 255`, `, "route": {"final": "wan", "default_mark": 8227}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			in := ""
			if c.inbound != "" {
				in = strings.Replace(c.inbound, "%s", "", 1)
			}
			doc := `{"inbounds": [` + in + `], "outbounds": [` + strings.Replace(direct, "%s", c.outbound, 1) + `]` + c.route + `}`
			proxyPath := filepath.Join(dir, "proxy.json")
			if err := os.WriteFile(proxyPath, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			conf := strings.Replace(e2eConfig, "config ipv6 'ipv6'", "config proxy 'proxy'\n\toption enabled '1'\n\toption transparent_mode '"+c.mode+"'\n\toption config_file '"+proxyPath+"'\n\nconfig ipv6 'ipv6'", 1)
			cfgPath := filepath.Join(dir, "swanix.conf")
			if err := os.WriteFile(cfgPath, []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(config.ProfileFull); err != nil {
				t.Fatal(err)
			}
			err = checkProxy(cfg)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}
