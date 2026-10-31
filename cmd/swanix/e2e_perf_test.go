package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

// TestPerfDataPath measures the data path through the router with ci/test/perf/perfload.
// Three paired comparisons, each run as alternating rounds (A B, B A, ...) so that drift
// of the machine affects both sides alike: the same single path without and with swanix
// (its own cost), kernel ECMP over two lines without swanix against swanix's balancing,
// and per-connection balancing against source+destination stickiness with 100 clients.
// Then failover convergence. A measurement, not a gate: it runs only with SWANIX_PERF=1;
// SWANIX_PERF_ROUNDS sets the rounds per comparison (default 3) and
// SWANIX_PERF_REPORT=<file> writes the results as JSON. Absolute numbers depend on the
// machine (veth links, one kernel); the paired changes are the result.
func TestPerfDataPath(t *testing.T) {
	nstest.Require(t)
	if os.Getenv("SWANIX_PERF") != "1" {
		t.Skip("set SWANIX_PERF=1")
	}
	_ = nstest.New(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "swanix")
	load := filepath.Join(dir, "perfload")
	for _, b := range []struct{ out, pkg, tags string }{{bin, ".", "mw_lite"}, {load, "../../ci/test/perf/perfload", ""}} {
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
	time.Sleep(500 * time.Millisecond)

	report := map[string]any{"cpus": runtime.NumCPU(), "kernel": strings.TrimSpace(nstest.Sh(t, "uname", "-r")),
		"health": "interval 1 s, timeout 1 s, down_rounds 2, up_rounds 2"}
	var rows []map[string]any
	run := func(mode string, args ...string) map[string]any {
		t.Helper()
		out := nstest.NS(t, "lan", append([]string{load, mode, "-target", "203.0.113.10:9000"}, args...)...)
		var r map[string]any
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("perfload %s: %v\n%s", mode, err, out)
		}
		return r
	}
	rounds := 3
	if n, err := fmt.Sscan(os.Getenv("SWANIX_PERF_ROUNDS"), &rounds); n != 1 || err != nil || rounds < 1 {
		rounds = 3
	}
	report["rounds"] = rounds
	start := func(cfg string) (pid int, stop func()) {
		t.Helper()
		cfgPath := filepath.Join(dir, "swanix.conf")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SWANIX_CONFIG", cfgPath)
		d := exec.Command(bin, "run")
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "lines healthy", 30*time.Second, func() bool {
			st, ok := status(t, bin)
			if !ok {
				return false
			}
			n := 0
			for _, w := range st.WANs {
				if w.Families[0].State == "healthy" {
					n++
				}
			}
			return n == 2
		})
		time.Sleep(3 * time.Second)
		return d.Process.Pid, func() { _ = d.Process.Signal(syscall.SIGTERM); _ = d.Wait() }
	}
	// clean: no swanix objects at all (the daemon leaves them in place on SIGTERM)
	clean := func() { _ = exec.Command(bin, "remove").Run() }

	// A variant is one way of running the router; up() prepares it and returns the daemon's
	// PID (0 without swanix) and a teardown.
	type variant struct {
		name string
		up   func() (int, func())
		src  string
	}
	noSwanix := func(route ...string) func() (int, func()) {
		return func() (int, func()) {
			clean()
			nstest.Sh(t, "ip", append([]string{"route", "replace"}, route...)...)
			return 0, func() { nstest.Sh(t, "ip", "route", "replace", "default", "via", "192.0.2.1", "dev", "eth1") }
		}
	}
	withSwanix := func(cfg string) func() (int, func()) {
		return func() (int, func()) { clean(); return start(cfg) }
	}
	round := func(v variant) (c, b map[string]any, cpu float64) {
		t.Helper()
		pid, down := v.up()
		defer down()
		cargs := []string{"-c", "32", "-d", "8s"}
		bargs := []string{"-streams", "8", "-d", "8s"}
		if v.src != "" {
			cargs, bargs = append(cargs, "-src", v.src), append(bargs, "-src", v.src)
		}
		var cpu0 float64
		if pid > 0 {
			cpu0 = cpuTicks(t, pid)
		}
		c, b = run("connect", cargs...), run("bulk", bargs...)
		if pid > 0 {
			cpu = (cpuTicks(t, pid) - cpu0) / 100
		}
		return c, b, cpu
	}
	medianOf := func(v []float64) float64 {
		w := append([]float64(nil), v...)
		sort.Float64s(w)
		return w[len(w)/2]
	}
	num := func(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }

	// paired runs the two variants alternately (A B, B A, A B, ...) so slow drift of the
	// machine affects both alike, and reports medians and the median of the per-round
	// ratios B/A.
	paired := func(title string, va, vb variant) {
		t.Helper()
		type acc struct {
			cps, p50, p99, gbps, cpu []float64
			errs                     int
			lines                    any
		}
		res := map[string]*acc{va.name: {}, vb.name: {}}
		var rc, rg []float64
		for i := 0; i < rounds; i++ {
			order := []variant{va, vb}
			if i%2 == 1 {
				order = []variant{vb, va}
			}
			got := map[string][2]float64{}
			for _, v := range order {
				c, b, cpu := round(v)
				r := res[v.name]
				r.cps, r.p50, r.p99 = append(r.cps, num(c, "connections_per_second")), append(r.p50, num(c, "latency_p50_ms")), append(r.p99, num(c, "latency_p99_ms"))
				r.gbps, r.cpu, r.lines = append(r.gbps, num(b, "gbit_per_second")), append(r.cpu, cpu), c["per_line"]
				for _, e := range c["errors"].(map[string]any) {
					r.errs += int(e.(float64))
				}
				got[v.name] = [2]float64{num(c, "connections_per_second"), num(b, "gbit_per_second")}
			}
			rc = append(rc, got[vb.name][0]/got[va.name][0])
			rg = append(rg, got[vb.name][1]/got[va.name][1])
		}
		for _, v := range []variant{va, vb} {
			r := res[v.name]
			row := map[string]any{"comparison": title, "scenario": v.name, "connections_per_second": medianOf(r.cps),
				"latency_p50_ms": medianOf(r.p50), "latency_p99_ms": medianOf(r.p99), "gbit_per_second": medianOf(r.gbps),
				"connect_errors": r.errs, "daemon_cpu_seconds_per_round": medianOf(r.cpu)}
			rows = append(rows, row)
			t.Logf("PERF %-44s median of %d: %8.0f conn/s  p50 %5.2f ms  p99 %6.2f ms  errors %d  lines %v | bulk %6.2f Gbit/s | daemon CPU %.2f s/round",
				v.name, rounds, medianOf(r.cps), medianOf(r.p50), medianOf(r.p99), r.errs, r.lines, medianOf(r.gbps), medianOf(r.cpu))
		}
		dc, dg := 100*(medianOf(rc)-1), 100*(medianOf(rg)-1)
		rows = append(rows, map[string]any{"comparison": title, "paired_rounds": rounds,
			"connections_per_second_change_percent": dc, "throughput_change_percent": dg})
		t.Logf("PERF %-44s paired change of %q vs %q: connections/s %+.1f%%, throughput %+.1f%%", title, vb.name, va.name, dc, dg)
	}

	var ips []string
	for i := 0; i < 100; i++ {
		ip := fmt.Sprintf("192.168.1.%d", 150+i)
		nstest.NS(t, "lan", "ip", "addr", "add", ip+"/24", "dev", "eth0")
		ips = append(ips, ip)
	}
	src := strings.Join(ips, ",")
	single := strings.Replace(e2eConfig, "config wan 'wan2'\n\toption id '2'", "config wan 'wan2'\n\toption id '2'\n\toption role 'backup'", 1)
	sticky := strings.Replace(e2eConfig, "\tlist member 'wan2'\n", "\tlist member 'wan2'\n\toption affinity 'source_destination'\n", 1)
	nstest.Sh(t, "sysctl", "-qw", "net.ipv4.fib_multipath_hash_policy=1")

	// 1. swanix's own cost: the same single path with and without its rules
	paired("overhead on one line",
		variant{name: "no swanix, main via wan1", up: noSwanix("default", "via", "192.0.2.1", "dev", "eth1")},
		variant{name: "swanix, one active line", up: withSwanix(single)})
	// 2. two lines: kernel ECMP (L4 hash) without swanix against swanix's balancing
	paired("two lines",
		variant{name: "no swanix, kernel ECMP over both lines", up: noSwanix("default", "nexthop", "via", "192.0.2.1", "dev", "eth1", "nexthop", "via", "198.51.100.1", "dev", "eth2")},
		variant{name: "swanix, two lines balanced", up: withSwanix(e2eConfig)})
	// 3. stickiness: 100 clients, per connection against source+destination
	paired("stickiness",
		variant{name: "swanix, balanced, 100 clients", up: withSwanix(e2eConfig), src: src},
		variant{name: "swanix, stickiness src+dst, 100 clients", up: withSwanix(sticky), src: src})
	elems := 0
	for _, set := range []string{"affset_balanced_1_4", "affset_balanced_2_4"} {
		out, _ := exec.Command("nft", "list", "set", "inet", "swanix", set).CombinedOutput()
		elems += strings.Count(string(out), " expires ")
	}
	t.Logf("PERF stickiness entries in the kernel sets after the last round: %d", elems)

	// 4. failover convergence: from the event until a burst of new connections all succeed on wan1
	clean()
	pid, stop := start(e2eConfig)
	converge := func(name string, breakIt, repair func()) {
		t.Helper()
		t0 := time.Now()
		breakIt()
		var down, zero time.Duration
		for time.Since(t0) < 60*time.Second {
			if st, ok := status(t, bin); ok {
				for _, w := range st.WANs {
					if w.Name != "wan2" {
						continue
					}
					if down == 0 && w.Families[0].State == "down" {
						down = time.Since(t0)
					}
					if zero == 0 && w.Families[0].Share["balanced"] == 0 {
						zero = time.Since(t0)
					}
				}
			}
			r := run("connect", "-c", "8", "-d", "300ms", "-timeout", "500ms")
			pl, errs := r["per_line"].(map[string]any), r["errors"].(map[string]any)
			if len(errs) == 0 && pl["wan2"] == nil && pl["wan1"] != nil {
				conv := time.Since(t0)
				rows = append(rows, map[string]any{"scenario": name, "down_after_seconds": down.Seconds(),
					"share_zero_after_seconds": zero.Seconds(), "converged_after_seconds": conv.Seconds()})
				t.Logf("PERF %-40s down after %.1f s, share 0 after %.1f s, new connections all on wan1 and error-free after %.1f s",
					name, down.Seconds(), zero.Seconds(), conv.Seconds())
				repair()
				waitFor(t, "wan2 healthy again", 60*time.Second, func() bool {
					st, _ := status(t, bin)
					s, _ := state(st, "wan2")
					return s == "healthy"
				})
				return
			}
		}
		t.Fatalf("%s: no convergence within 60 s", name)
	}
	converge("failover: upstream blackholed",
		func() {
			nstest.NS(t, "isp2", "nft", "add", "table", "inet", "bh")
			nstest.NS(t, "isp2", "nft", "add", "chain", "inet", "bh", "in", "{ type filter hook input priority 0; policy drop; }")
		},
		func() { nstest.NS(t, "isp2", "nft", "delete", "table", "inet", "bh") })
	// netifd reports the interface down when its link drops; the stub follows the same order
	wan2JSON := filepath.Join(dir, "wan2.json")
	converge("failover: link down (netlink + netifd)",
		func() {
			if err := os.WriteFile(wan2JSON, []byte(`{"up":false,"available":true,"l3_device":"eth2"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			nstest.Sh(t, "ip", "link", "set", "eth2", "down")
		},
		func() {
			nstest.Sh(t, "ip", "link", "set", "eth2", "up")
			if err := os.WriteFile(wan2JSON, []byte(ifaceWAN2), 0o644); err != nil {
				t.Fatal(err)
			}
			_ = exec.Command(bin, "reconcile").Run()
		})
	_ = pid
	stop()
	_ = exec.Command(bin, "remove").Run()

	report["results"] = rows
	if p := os.Getenv("SWANIX_PERF_REPORT"); p != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
