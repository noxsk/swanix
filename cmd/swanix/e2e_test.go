package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
	"github.com/sagernet/sing-box/experimental/multiwan/telemetry"
)

const ifaceWAN = `{"up":true,"available":true,"l3_device":"eth1","ipv4-address":[{"address":"192.0.2.2","mask":24}],
"route":[{"target":"0.0.0.0","mask":0,"nexthop":"192.0.2.1"}]}`
const ifaceWAN2 = `{"up":true,"available":true,"l3_device":"eth2","ipv4-address":[{"address":"198.51.100.2","mask":24}],
"route":[{"target":"0.0.0.0","mask":0,"nexthop":"198.51.100.1"}]}`
const ifaceLAN = `{"up":true,"available":true,"l3_device":"br-lan","ipv4-address":[{"address":"192.168.1.1","mask":24}]}`

const proxyJSON = `{
  "log": {"level": "warn"},
  "inbounds": [{"type": "socks", "tag": "in", "listen": "192.168.1.1", "listen_port": 1080}],
  "outbounds": [{"type": "direct", "tag": "wan", "wan_policy": "balanced"}],
  "route": {"final": "wan"}
}`

const e2eConfig = `
config global 'global'
	option config_version '1'
	option enabled '1'
	option startup_mode 'manage'
	list lan_network 'lan'
	option default_policy 'balanced'
	option update_interval '2'
	option sample_interval '1'

config wan 'wan1'
	option id '1'
	option network 'wan'
	option ipv6_network 'none'
	option download_mbps '100'
	option upload_mbps '100'
	option traffic_bias 'download'
	option health 'fast'

config wan 'wan2'
	option id '2'
	option network 'wan2'
	option ipv6_network 'none'
	option download_mbps '100'
	option upload_mbps '100'
	option traffic_bias 'download'
	option health 'fast'

config health 'fast'
	list target '203.0.113.10:80'
	option interval '1'
	option timeout '1'
	option successes_required '1'
	option down_rounds '2'
	option up_rounds '2'
	option recover_ramp '4'

config policy 'balanced'
	list member 'wan1'
	list member 'wan2'

config ipv6 'ipv6'
	option mode 'unmanaged'
`

func status(t *testing.T, bin string) (telemetry.Status, bool) {
	t.Helper()
	cmd := exec.Command(bin, "status", "--json")
	out, err := cmd.Output()
	if err != nil {
		return telemetry.Status{}, false
	}
	var st telemetry.Status
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatalf("status json: %v\n%s", err, out)
	}
	return st, true
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func state(st telemetry.Status, wan string) (string, int) {
	for _, w := range st.WANs {
		if w.Name == wan {
			return w.Families[0].State, w.Families[0].Share["balanced"]
		}
	}
	return "", 0
}

// TestLiteDaemonEndToEnd runs the real Lite binary against the namespace
// topology: probes through per-WAN routing tables, failover, recovery.
func TestLiteDaemonEndToEnd(t *testing.T) {
	nstest.Require(t)
	tp := nstest.New(t)
	dir := t.TempDir()

	bin := filepath.Join(dir, "swanix")
	build := exec.Command("go", "build", "-tags", "mw_lite", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	cfgPath := filepath.Join(dir, "swanix.conf")
	if err := os.WriteFile(cfgPath, []byte(e2eConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWANIX_CONFIG", cfgPath)

	if out, err := exec.Command(bin, "check").CombinedOutput(); err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}

	var logs bytes.Buffer
	daemon := exec.Command(bin, "run")
	daemon.Stdout, daemon.Stderr = &logs, &logs
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = daemon.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(syscall.SIGKILL)
		<-done
		if t.Failed() {
			t.Logf("daemon log:\n%s", logs.String())
		}
	})

	waitFor(t, "both WANs healthy", 20*time.Second, func() bool {
		st, ok := status(t, bin)
		if !ok {
			return false
		}
		a, sa := state(st, "wan1")
		b, sb := state(st, "wan2")
		return a == "healthy" && b == "healthy" && sa > 0 && sb > 0
	})

	t.Run("balanced across both lines", func(t *testing.T) {
		r := tp.Count("lan", 200)
		if r["wan1"]+r["wan2"] != 200 {
			t.Fatalf("lost: %v", r)
		}
		nstest.Within(t, "wan1 share", r["wan1"], 200, 0.50, 0.15)
	})

	t.Run("link down is noticed from netlink without waiting for probes", func(t *testing.T) {
		down := strings.Replace(ifaceWAN2, `"up":true`, `"up":false`, 1)
		if err := os.WriteFile(filepath.Join(dir, "wan2.json"), []byte(down), 0o644); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		nstest.Sh(t, "ip", "link", "set", "eth2", "down")
		waitFor(t, "wan2 down via link event", 10*time.Second, func() bool {
			st, _ := status(t, bin)
			s, share := state(st, "wan2")
			_, share1 := state(st, "wan1")
			return s == "down" && share == 0 && share1 == 100
		})
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("link event took %v; probes alone would need about 3 s, the event path must not be slower", d)
		}
		if r := tp.Count("lan", 20); r["wan1"] != 20 {
			t.Fatalf("traffic still uses the dead line: %v", r)
		}
		// restore: link up alone must not make the line usable; probes bring it back
		if err := os.WriteFile(filepath.Join(dir, "wan2.json"), []byte(ifaceWAN2), 0o644); err != nil {
			t.Fatal(err)
		}
		nstest.Sh(t, "ip", "link", "set", "eth2", "up")
		waitFor(t, "wan2 healthy again after link up", 40*time.Second, func() bool {
			st, _ := status(t, bin)
			s, share := state(st, "wan2")
			return s == "healthy" && share >= 35
		})
	})

	t.Run("failover when a line's upstream dies", func(t *testing.T) {
		tp.StopServer("isp2", "203.0.113.10")
		waitFor(t, "wan2 down and out of the pool", 25*time.Second, func() bool {
			st, _ := status(t, bin)
			s, share := state(st, "wan2")
			_, share1 := state(st, "wan1")
			return s == "down" && share == 0 && share1 == 100
		})
		// the router itself keeps working too, and only over wan1
		if r := tp.Count("", 30); r["wan1"] != 30 {
			t.Fatalf("router traffic after failover: %v", r)
		}
		// isp2's server is gone, so a LAN connection can only have been served by wan1
		if r := tp.Count("lan", 30); r["wan1"] != 30 {
			t.Fatalf("lan traffic after failover: %v", r)
		}
		st, _ := status(t, bin)
		if len(st.Transitions) == 0 {
			t.Fatal("transition not recorded")
		}
	})

	t.Run("recovery ramps back in", func(t *testing.T) {
		tp.StartServer("isp2", "wan2", "203.0.113.10", 80)
		waitFor(t, "wan2 back in the pool", 40*time.Second, func() bool {
			st, _ := status(t, bin)
			_, share := state(st, "wan2")
			return share > 0
		})
		waitFor(t, "wan2 healthy again", 40*time.Second, func() bool {
			st, _ := status(t, bin)
			s, share := state(st, "wan2")
			return s == "healthy" && share >= 35
		})
		r := tp.Count("lan", 100)
		if r["wan2"] == 0 || r["wan1"] == 0 {
			t.Fatalf("after recovery both lines should carry traffic: %v", r)
		}
	})

	t.Run("cli", func(t *testing.T) {
		for _, args := range [][]string{{"status"}, {"preview"}, {"reconcile"}, {"features", "--json"}, {"version"}} {
			if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
				t.Errorf("%v: %v\n%s", args, err, out)
			}
		}
		out, _ := exec.Command(bin, "preview").Output()
		if !strings.Contains(string(out), "ip -4 rule add fwmark 0x01000000/0x0f000000 priority 10201 lookup 1001") {
			t.Errorf("preview lacks the business rule:\n%s", out)
		}
	})

	t.Run("daemon exit leaves kernel state, remove withdraws it", func(t *testing.T) {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not exit on SIGTERM")
		}
		if out := nstest.Sh(t, "nft", "list", "tables"); !strings.Contains(out, " swanix") {
			t.Fatal("rules should stay in place while the daemon is down (last state kept)")
		}
		if out, err := exec.Command(bin, "remove").CombinedOutput(); err != nil {
			t.Fatalf("remove: %v\n%s", err, out)
		}
		if out := nstest.Sh(t, "nft", "list", "tables"); strings.Contains(out, " swanix") {
			t.Fatal("remove left the table behind")
		}
		if got := strings.TrimSpace(nstest.Sh(t, "cat", "/proc/sys/net/ipv4/conf/eth1/rp_filter")); got != "1" {
			t.Fatalf("rp_filter not restored: %s", got)
		}
	})
}

func parseCounts(t *testing.T, out string) map[string]int {
	t.Helper()
	res := map[string]int{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("bad output %q: %v", out, err)
	}
	return res
}
