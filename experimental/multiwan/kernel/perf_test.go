package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

// TestPerfWeightUpdatesUnderLoad applies 100 weight changes (one atomic nft batch each)
// while new connections and bulk downloads run through the router, and reports how long
// an update takes and whether any connection failed or any download stalled. A full
// re-apply (table, rules and routes) is timed as well. Runs only with SWANIX_PERF=1.
func TestPerfWeightUpdatesUnderLoad(t *testing.T) {
	nstest.Require(t)
	if os.Getenv("SWANIX_PERF") != "1" {
		t.Skip("set SWANIX_PERF=1")
	}
	_ = nstest.New(t)
	load := filepath.Join(t.TempDir(), "perfload")
	if out, err := exec.Command("go", "build", "-o", load, "../../../ci/test/perf/perfload").CombinedOutput(); err != nil {
		t.Fatalf("build perfload: %v\n%s", err, out)
	}
	for i, ns := range []string{"isp1", "isp2"} {
		srv := exec.Command("ip", "netns", "exec", ns, load, "serve", "-listen", "203.0.113.10:9000", "-banner", fmt.Sprintf("wan%d", i+1))
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	}
	_ = exec.Command("ip", "netns", "exec", "lan", "sysctl", "-qw", "net.ipv4.ip_local_port_range=1024 65000").Run()
	time.Sleep(500 * time.Millisecond)

	ap := NewApplier()
	ap.StateDir = t.TempDir()
	in := fixture(t)
	in.Cfg.IPv6.Mode = config.IPv6Unmanaged
	in.WANs[0].Up6, in.WANs[1].Up6 = false, false
	in.Shares = Shares{"balanced": {4: {1: 50, 2: 50}}}
	if _, err := ap.Apply(in); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ap.Remove(in) }()

	type result struct {
		out []byte
		err error
	}
	start := func(args ...string) chan result {
		ch := make(chan result, 1)
		go func() {
			out, err := exec.Command("ip", append([]string{"netns", "exec", "lan", load}, args...)...).Output()
			ch <- result{out, err}
		}()
		return ch
	}
	connectCh := start("connect", "-target", "203.0.113.10:9000", "-c", "16", "-d", "12s")
	bulkCh := start("bulk", "-target", "203.0.113.10:9000", "-streams", "4", "-d", "12s")
	time.Sleep(time.Second)

	var updates []time.Duration
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			in.Shares["balanced"][4] = map[int]int{1: 70, 2: 30}
		} else {
			in.Shares["balanced"][4] = map[int]int{1: 30, 2: 70}
		}
		t0 := time.Now()
		if err := ap.UpdateWeights(in); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		updates = append(updates, time.Since(t0))
		time.Sleep(80 * time.Millisecond)
	}
	var applies []time.Duration
	for i := 0; i < 10; i++ {
		t0 := time.Now()
		if _, err := ap.Apply(in); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		applies = append(applies, time.Since(t0))
		time.Sleep(100 * time.Millisecond)
	}

	c, b := <-connectCh, <-bulkCh
	if c.err != nil || b.err != nil {
		t.Fatalf("load generators failed: %v %v", c.err, b.err)
	}
	var cr, br map[string]any
	if err := json.Unmarshal(c.out, &cr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b.out, &br); err != nil {
		t.Fatal(err)
	}
	stat := func(d []time.Duration) (p50, p99, max float64) {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		ms := func(x time.Duration) float64 { return float64(x) / float64(time.Millisecond) }
		return ms(d[len(d)/2]), ms(d[len(d)*99/100]), ms(d[len(d)-1])
	}
	u50, u99, umax := stat(updates)
	a50, _, amax := stat(applies)
	t.Logf("PERF weight update (one nft batch), 100 under load: p50 %.1f ms, p99 %.1f ms, max %.1f ms", u50, u99, umax)
	t.Logf("PERF full re-apply (table + rules + routes), 10 under load: p50 %.1f ms, max %.1f ms", a50, amax)
	t.Logf("PERF load during the updates: %v conn/s, errors %v, lines %v | bulk %v Gbit/s, slowest stream %v Gbit/s, errors %v",
		cr["connections_per_second"], cr["errors"], cr["per_line"], br["gbit_per_second"], br["slowest_stream_gbit_per_second"], br["errors"])
	if errs := cr["errors"].(map[string]any); len(errs) != 0 {
		t.Errorf("connections failed while weights changed: %v", errs)
	}
	if errs := br["errors"].(map[string]any); len(errs) != 0 {
		t.Errorf("downloads broke while weights changed: %v", errs)
	}
	if v, _ := br["slowest_stream_gbit_per_second"].(float64); v <= 0 {
		t.Errorf("a download stalled while weights changed")
	}
	if p := os.Getenv("SWANIX_PERF_REPORT_KERNEL"); p != "" {
		rep := map[string]any{"weight_update_ms": map[string]float64{"p50": u50, "p99": u99, "max": umax},
			"full_apply_ms": map[string]float64{"p50": a50, "max": amax}, "connect": cr, "bulk": br}
		bb, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(p, bb, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
