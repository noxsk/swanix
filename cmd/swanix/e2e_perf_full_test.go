//go:build !mw_lite

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

// TestPerfFullTUN compares the same LAN load through the kernel path (Full binary,
// proxy off) and through the proxy (TUN with auto_route, direct outbound with
// wan_policy), and reports the daemon's CPU time, which is the cost of carrying the
// traffic in user space. Runs only with SWANIX_PERF=1; SWANIX_PERF_REPORT_FULL=<file>
// writes JSON.
func TestPerfFullTUN(t *testing.T) {
	nstest.Require(t)
	if os.Getenv("SWANIX_PERF") != "1" {
		t.Skip("set SWANIX_PERF=1")
	}
	_ = nstest.New(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "swanix")
	load := filepath.Join(dir, "perfload")
	// SWANIX_PERF_TAGS: build tags of the Full binary (default with_gvisor, which the
	// release build also has; the gvisor stack needs it)
	tags := os.Getenv("SWANIX_PERF_TAGS")
	if tags == "" {
		tags = "with_gvisor"
	}
	for _, b := range []struct{ out, pkg, tags string }{{bin, ".", tags}, {load, "../../ci/test/perf/perfload", ""}} {
		cmd := exec.Command("go", "build", "-tags", b.tags, "-o", b.out, b.pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", b.pkg, err, out)
		}
	}
	nstest.FakeUbus(t, dir, map[string]string{"wan": ifaceWAN, "wan2": ifaceWAN2, "lan": ifaceLAN})
	for i, ns := range []string{"isp1", "isp2"} {
		srv := exec.Command("ip", "netns", "exec", ns, load, "serve", "-listen", "203.0.113.10:9000", "-banner", fmt.Sprintf("wan%d", i+1))
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	}
	_ = exec.Command("ip", "netns", "exec", "lan", "sysctl", "-qw", "net.ipv4.ip_local_port_range=1024 65000").Run()
	proxyPath := filepath.Join(dir, "proxy.json")
	// SWANIX_PERF_TUN_STACK (system, gvisor, mixed) and SWANIX_PERF_TUN_MTU select the TUN
	// variant. Default: gvisor with MTU 1500, the fastest variant measured (sing-tun enables
	// GSO only for gvisor below MTU 49152; the system stack forwards packet by packet).
	stack, mtu := os.Getenv("SWANIX_PERF_TUN_STACK"), os.Getenv("SWANIX_PERF_TUN_MTU")
	if stack == "" {
		stack = "gvisor"
	}
	if mtu == "" {
		mtu = "1500"
	}
	tunJSON := fmt.Sprintf(`{"log": {"level": "error"},
  "inbounds": [{"type": "tun", "tag": "tun-in", "interface_name": "swtun0", "address": ["172.19.0.1/30"], "auto_route": true, "stack": %q, "mtu": %s}],
  "outbounds": [{"type": "direct", "tag": "wan", "wan_policy": "balanced"}],
  "route": {"final": "wan"}}`, stack, mtu)
	if err := os.WriteFile(proxyPath, []byte(tunJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	var rows []map[string]any
	for _, c := range []struct{ name, proxy string }{
		{"Full, kernel path (proxy off)", ""},
		{"Full, TUN (" + stack + ", mtu " + mtu + ") + wan_policy", "config proxy 'proxy'\n\toption enabled '1'\n\toption transparent_mode 'tun'\n\toption config_file '" + proxyPath + "'\n\n"},
	} {
		cfgPath := filepath.Join(dir, "swanix.conf")
		if err := os.WriteFile(cfgPath, []byte(strings.Replace(e2eConfig, "config ipv6 'ipv6'", c.proxy+"config ipv6 'ipv6'", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SWANIX_CONFIG", cfgPath)
		d := exec.Command(bin, "run")
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "lines healthy", 40*time.Second, func() bool {
			st, ok := status(t, bin)
			if !ok {
				return false
			}
			a, _ := state(st, "wan1")
			b, _ := state(st, "wan2")
			return a == "healthy" && b == "healthy"
		})
		time.Sleep(3 * time.Second)
		row := map[string]any{"scenario": c.name}
		for _, m := range []struct {
			mode string
			args []string
		}{
			{"connect", []string{"-c", "32", "-d", "8s"}},
			{"bulk", []string{"-streams", "8", "-d", "8s"}},
		} {
			cpu0 := cpuTicks(t, d.Process.Pid)
			out := nstest.NS(t, "lan", append([]string{load, m.mode, "-target", "203.0.113.10:9000"}, m.args...)...)
			var r map[string]any
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				t.Fatalf("perfload: %v\n%s", err, out)
			}
			r["daemon_cpu_seconds"] = (cpuTicks(t, d.Process.Pid) - cpu0) / 100
			row[m.mode] = r
		}
		rss, _ := memKB(t, d.Process.Pid)
		row["daemon_rss_mib"] = float64(rss) / 1024
		rows = append(rows, row)
		cr, br := row["connect"].(map[string]any), row["bulk"].(map[string]any)
		t.Logf("PERF %-36s %8.0f conn/s p50 %5.2f ms p99 %6.2f ms errors %v lines %v (daemon CPU %.1f s) | bulk %6.2f Gbit/s lines(MB) %v (daemon CPU %.1f s) | RSS %.0f MiB",
			c.name, cr["connections_per_second"], cr["latency_p50_ms"], cr["latency_p99_ms"], cr["errors"], cr["per_line"], cr["daemon_cpu_seconds"],
			br["gbit_per_second"], br["per_line_mbytes"], br["daemon_cpu_seconds"], row["daemon_rss_mib"])
		_ = d.Process.Signal(syscall.SIGTERM)
		_ = d.Wait()
	}
	_ = exec.Command(bin, "remove").Run()
	if p := os.Getenv("SWANIX_PERF_REPORT_FULL"); p != "" {
		b, _ := json.MarshalIndent(map[string]any{"results": rows}, "", "  ")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
