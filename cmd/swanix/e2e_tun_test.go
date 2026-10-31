//go:build !mw_lite

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

// The proxy takes the LAN's traffic over a TUN with auto_route (transparent_mode tun).
// One LAN address is routed to an outbound pinned to wan2 while the kernel path would
// balance it: answers from wan2 only show that its traffic went through the TUN and
// the proxy. Another address uses the balanced policy and has to follow failover.
const tunProxyJSON = `{
  "log": {"level": "warn"},
  "inbounds": [{"type": "tun", "tag": "tun-in", "interface_name": "swtun0", "address": ["172.19.0.1/30"],
    "auto_route": true%s, "stack": "system"}],
  "outbounds": [
    {"type": "direct", "tag": "wan", "wan_policy": "balanced"},
    {"type": "direct", "tag": "pinned", "wan_policy": "wan2_only"}],
  "route": {"rules": [{"source_ip_cidr": ["192.168.1.100/32"], "outbound": "pinned"}], "final": "wan"}
}`

func TestFullTUNTakesLANTraffic(t *testing.T) {
	nstest.Require(t)
	tp := nstest.New(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "swanix")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	nstest.NS(t, "lan", "ip", "addr", "add", "192.168.1.120/24", "dev", "eth0")
	for _, v := range []struct{ name, extra, reject string }{
		{"auto_route", "", ""},
		// its redirect would pull probes and wan_policy sockets back into the proxy
		{"auto_redirect is refused", `, "auto_redirect": true`, "auto_redirect"},
	} {
		t.Run(v.name, func(t *testing.T) {
			proxyPath := filepath.Join(dir, "proxy.json")
			if err := os.WriteFile(proxyPath, []byte(strings.Replace(tunProxyJSON, "%s", v.extra, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := strings.Replace(e2eConfig, "config ipv6 'ipv6'", "config proxy 'proxy'\n\toption enabled '1'\n\toption transparent_mode 'tun'\n\toption config_file '"+proxyPath+"'\n\nconfig policy 'wan2_only'\n\tlist member 'wan2'\n\nconfig ipv6 'ipv6'", 1)
			cfgPath := filepath.Join(dir, "swanix.conf")
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SWANIX_CONFIG", cfgPath)
			out, err := exec.Command(bin, "check").CombinedOutput()
			if v.reject != "" {
				if err == nil || !strings.Contains(string(out), v.reject) {
					t.Fatalf("check accepted it: %v\n%s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("check: %v\n%s", err, out)
			}
			var logs syncBuffer
			daemon := exec.Command(bin, "run")
			daemon.Stdout, daemon.Stderr = &logs, &logs
			if err := daemon.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { _ = daemon.Wait(); close(done) }()
			defer func() {
				_ = daemon.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					_ = daemon.Process.Signal(syscall.SIGKILL)
					<-done
				}
				if t.Failed() {
					t.Logf("daemon log:\n%s", logs.String())
					t.Logf("rules:\n%s", nstest.Sh(t, "ip", "rule"))
				}
			}()
			waitFor(t, "both WANs healthy with the TUN up", 30*time.Second, func() bool {
				st, ok := status(t, bin)
				if !ok {
					return false
				}
				a, _ := state(st, "wan1")
				b, _ := state(st, "wan2")
				return a == "healthy" && b == "healthy"
			})
			if r := tp.Count("lan", 30, "0", "192.168.1.100"); r["wan2"] != 30 {
				t.Fatalf("pinned LAN client did not go through the TUN and the proxy: %v", r)
			}
			r := tp.Count("lan", 100, "0", "192.168.1.120")
			if r["wan1"]+r["wan2"] != 100 {
				t.Fatalf("lost connections through the TUN: %v", r)
			}
			nstest.Within(t, "wan1 share through the TUN", r["wan1"], 100, 0.50, 0.17)
			if r := tp.Count("", 30); r["wan1"]+r["wan2"] != 30 {
				t.Fatalf("router traffic broken with the TUN up: %v", r)
			}
			tp.StopServer("isp2", "203.0.113.10")
			waitFor(t, "wan2 down", 25*time.Second, func() bool {
				st, _ := status(t, bin)
				s, _ := state(st, "wan2")
				return s == "down"
			})
			if r := tp.Count("lan", 30, "0", "192.168.1.120"); r["wan1"] != 30 {
				t.Fatalf("TUN traffic still reached the dead line: %v", r)
			}
			tp.StartServer("isp2", "wan2", "203.0.113.10", 80)
			waitFor(t, "wan2 healthy again", 30*time.Second, func() bool {
				st, _ := status(t, bin)
				s, _ := state(st, "wan2")
				return s == "healthy"
			})
		})
	}
}
