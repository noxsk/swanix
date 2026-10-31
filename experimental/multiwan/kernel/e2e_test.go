package kernel

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/nstest"
)

// End-to-end through the real kernel: real TCP connections cross the generated
// data path of a router with two ISPs and a LAN (see nstest).

func TestEndToEnd(t *testing.T) {
	tp := nstest.New(t)
	ap := NewApplier()
	ap.StateDir = t.TempDir()
	in := fixture(t)
	in.Cfg.IPv6.Mode = "unmanaged"
	in.WANs[0].Up6, in.WANs[1].Up6 = false, false
	in.Shares = Shares{"balanced": {4: {1: 50, 2: 50}}}
	setShares := func(m map[int]int) {
		t.Helper()
		in.Shares["balanced"][4] = m
		if err := ap.UpdateWeights(in); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := ap.Apply(in); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Apply(in); err != nil {
		t.Fatalf("second apply (idempotence): %v", err)
	}

	t.Run("rp_filter loosened on managed WAN devices only", func(t *testing.T) {
		for dev, want := range map[string]string{"eth1": "2", "eth2": "2", "br-lan": "0"} {
			got := strings.TrimSpace(nstest.Sh(t, "cat", "/proc/sys/net/ipv4/conf/"+dev+"/rp_filter"))
			if got != want {
				t.Errorf("%s rp_filter = %s, want %s", dev, got, want)
			}
		}
	})

	t.Run("lan distribution follows weights", func(t *testing.T) {
		setShares(map[int]int{1: 70, 2: 30})
		r := tp.Count("lan", 300)
		if r["wan1"]+r["wan2"] != 300 {
			t.Fatalf("lost connections: %v", r)
		}
		nstest.Within(t, "wan1 share", r["wan1"], 300, 0.70, 0.10)
	})

	t.Run("runtime update keeps counters and reports assignments", func(t *testing.T) {
		setShares(map[int]int{1: 50, 2: 50})
		before, err := ap.Counters(in)
		if err != nil {
			t.Fatal(err)
		}
		r := tp.Count("lan", 100)
		if r["wan1"]+r["wan2"] != 100 {
			t.Fatalf("lost: %v", r)
		}
		in.LAN.Devices = []string{"br-lan", "br-guest"} // an extra LAN appears
		if err := ap.UpdateRuntime(in); err != nil {
			t.Fatalf("incremental update: %v", err)
		}
		after, err := ap.Counters(in)
		if err != nil {
			t.Fatal(err)
		}
		d1, d2 := after[1]-before[1], after[2]-before[2]
		if d1 != uint64(r["wan1"]) || d2 != uint64(r["wan2"]) {
			t.Fatalf("counters %d/%d disagree with observed %v", d1, d2, r)
		}
		if r = tp.Count("lan", 20); r["wan1"]+r["wan2"] != 20 {
			t.Fatalf("data path broken after incremental update: %v", r)
		}
	})

	t.Run("existing connection survives weight change", func(t *testing.T) {
		setShares(map[int]int{1: 100})
		first, again := tp.Persistent()
		setShares(map[int]int{2: 100})
		second := again()
		if first != "wan1" || second != "wan1" {
			t.Fatalf("established connection moved: %q then %q", first, second)
		}
		if r := tp.Count("lan", 40); r["wan2"] != 40 {
			t.Fatalf("new connections should all use wan2: %v", r)
		}
	})

	t.Run("no candidates rejects instead of falling back to main", func(t *testing.T) {
		setShares(map[int]int{})
		start := time.Now()
		r := tp.Count("lan", 5)
		// Replies are ICMP unreachables, which the kernel rate-limits, so some
		// clients see a timeout; what matters is that nothing leaves a WAN.
		if r["wan1"]+r["wan2"] != 0 {
			t.Fatalf("traffic leaked out: %v", r)
		}
		if r["err:ENETUNREACH"] == 0 {
			t.Fatalf("expected at least one explicit unreachable, got %v after %v", r, time.Since(start))
		}
	})

	t.Run("router traffic follows weights", func(t *testing.T) {
		setShares(map[int]int{1: 50, 2: 50})
		r := tp.Count("", 200)
		if r["wan1"]+r["wan2"] != 200 {
			t.Fatalf("lost: %v", r)
		}
		nstest.Within(t, "router wan1 share", r["wan1"], 200, 0.50, 0.12)
	})

	t.Run("probe traffic is pinned to its WAN", func(t *testing.T) {
		setShares(map[int]int{1: 100})
		lay, _ := NewLayout(in.Cfg.Kernel.MarkMask)
		mark := fmt.Sprint(lay.ProbeMark(2))
		if r := tp.Count("", 10, mark); r["wan2"] != 10 {
			t.Fatalf("probe for wan2 left via another line: %v", r)
		}
		// wan2 loses its route: its probe must fail, not fall over to wan1
		in.WANs[1].Up4 = false
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
		if r := tp.Count("", 3, mark); r["wan1"] != 0 || r["wan2"] != 0 {
			t.Fatalf("failed WAN probe leaked to another line: %v", r)
		}
		in.WANs[1].Up4 = true
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
		if r := tp.Count("", 3, mark); r["wan2"] != 3 {
			t.Fatalf("probe did not recover: %v", r)
		}
	})

	t.Run("inbound port forward replies via the arrival WAN", func(t *testing.T) {
		// make a plain reply take the wrong line unless inbound tracking works
		nstest.Sh(t, "ip", "route", "replace", "default", "via", "198.51.100.1", "dev", "eth2")
		defer nstest.Sh(t, "ip", "route", "replace", "default", "via", "192.0.2.1", "dev", "eth1")
		setShares(map[int]int{2: 100})
		out := nstest.NS(t, "isp1", "python3", "-I", "-c", `
import socket
s=socket.socket(); s.settimeout(3); s.bind(("203.0.113.50",0)); s.connect(("192.0.2.2",8080)); print(s.recv(64).decode().strip())`)
		if strings.TrimSpace(out) != "lan-server" {
			t.Fatalf("port forward failed: %q", out)
		}
	})

	t.Run("related ICMP and router services answer via the arrival WAN", func(t *testing.T) {
		// main points at the other line, so an unmarked reply would leave through eth2,
		// where isp2 has no route back to 203.0.113.50 and drops it
		nstest.Sh(t, "ip", "route", "replace", "default", "via", "198.51.100.1", "dev", "eth2")
		defer nstest.Sh(t, "ip", "route", "replace", "default", "via", "192.0.2.1", "dev", "eth1")
		setShares(map[int]int{2: 100})
		tp.StartServer("", "router-server", "192.0.2.2", 8081)
		defer tp.StopServer("", "192.0.2.2")
		for _, manage := range []bool{true, false} {
			in.Cfg.Global.ManageRouterTraffic = manage
			if _, err := ap.Apply(in); err != nil {
				t.Fatal(err)
			}
			// the LAN host's port-unreachable for a forwarded UDP port (RELATED to the inbound flow)
			if got := nstest.UDPProbe(t, "isp1", "203.0.113.50", "192.0.2.2", 9999); got != "refused" {
				t.Errorf("manage_router_traffic=%v: forwarded ICMP error: %s", manage, got)
			}
			// the router's own port-unreachable
			if got := nstest.UDPProbe(t, "isp1", "203.0.113.50", "192.0.2.2", 9998); got != "refused" {
				t.Errorf("manage_router_traffic=%v: router ICMP error: %s", manage, got)
			}
			// a TCP service on the router
			out := nstest.NS(t, "isp1", "python3", "-I", "-c", `
import socket
s=socket.socket(); s.settimeout(3); s.bind(("203.0.113.50",0))
try:
    s.connect(("192.0.2.2",8081)); print(s.recv(64).decode().strip())
except OSError as e:
    print("error", e)`)
			if strings.TrimSpace(out) != "router-server" {
				t.Errorf("manage_router_traffic=%v: router service: %q", manage, out)
			}
		}
		in.Cfg.Global.ManageRouterTraffic = true
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("source stickiness keeps a client on its line, clients still spread", func(t *testing.T) {
		in.Cfg.Policies[0].Affinity = "source"
		in.Cfg.Policies[0].AffinityTimeout = 300
		in.Shares["balanced"][4] = map[int]int{1: 50, 2: 50}
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		first := map[string]string{}
		for i := 0; i < 16; i++ {
			ip := fmt.Sprintf("192.168.1.%d", 110+i)
			nstest.NS(t, "lan", "ip", "addr", "add", ip+"/24", "dev", "eth0")
			r := tp.Count("lan", 12, "0", ip)
			if len(r) != 1 {
				t.Fatalf("client %s was split across lines (or failed): %v", ip, r)
			}
			for k := range r {
				seen[k] = true
				first[ip] = k
			}
		}
		if !seen["wan1"] || !seen["wan2"] {
			t.Fatalf("16 clients all landed on one line: %v", seen)
		}
		// a client whose line leaves the pool moves; no connection is sent to the dead line
		var victim, from string
		for ip, w := range first {
			victim, from = ip, w
			break
		}
		other := map[string]int{"wan1": 2, "wan2": 1}[from]
		in.Shares["balanced"][4] = map[int]int{other: 100}
		if err := ap.UpdateWeights(in); err != nil {
			t.Fatal(err)
		}
		r := tp.Count("lan", 10, "0", victim)
		if r[from] != 0 || r[map[int]string{1: "wan1", 2: "wan2"}[other]] != 10 {
			t.Fatalf("client %s did not leave %s: %v", victim, from, r)
		}
		in.Cfg.Policies[0].Affinity = "connection"
		in.Shares["balanced"][4] = map[int]int{1: 50, 2: 50}
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a sticky entry lives affinity_timeout after the last new connection, not after the first", func(t *testing.T) {
		in.Cfg.Policies[0].Affinity = "source"
		in.Cfg.Policies[0].AffinityTimeout = 2 // below the config minimum, to keep the test short
		in.Shares["balanced"][4] = map[int]int{1: 50, 2: 50}
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
		ip := "192.168.1.130"
		nstest.NS(t, "lan", "ip", "addr", "add", ip+"/24", "dev", "eth0")
		remembered := func() bool {
			out := nstest.Sh(t, "nft", "list", "set", "inet", "swanix", "affset_balanced_1_4") + nstest.Sh(t, "nft", "list", "set", "inet", "swanix", "affset_balanced_2_4")
			return strings.Contains(out, ip+" ")
		}
		var line string
		for i := 0; i < 5; i++ { // 5 s of activity, well past the 2 s timeout
			r := tp.Count("lan", 1, "0", ip)
			for k := range r {
				if line == "" {
					line = k
				}
				if k != line {
					t.Fatalf("an active client moved from %s to %s: its entry was not refreshed", line, k)
				}
			}
			time.Sleep(time.Second)
		}
		if !remembered() {
			t.Fatal("the entry of an active client expired")
		}
		time.Sleep(3 * time.Second)
		if remembered() {
			t.Fatal("the entry outlived affinity_timeout after the last connection")
		}
		in.Cfg.Policies[0].Affinity, in.Cfg.Policies[0].AffinityTimeout = "connection", 300
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("source+destination stickiness keeps each pair on its line, a client still uses both", func(t *testing.T) {
		tp.AddDestination("203.0.113.11")
		in.Cfg.Policies[0].Affinity = "source_destination"
		in.Cfg.Policies[0].AffinityTimeout = 300
		in.Shares["balanced"][4] = map[int]int{1: 50, 2: 50}
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
		split, lines := 0, map[string]bool{}
		for i := 0; i < 10; i++ {
			ip := fmt.Sprintf("192.168.1.%d", 140+i)
			nstest.NS(t, "lan", "ip", "addr", "add", ip+"/24", "dev", "eth0")
			got := map[string]string{}
			for _, dst := range []string{"203.0.113.10", "203.0.113.11"} {
				r := tp.CountTo("lan", dst, 10, "0", ip)
				if len(r) != 1 {
					t.Fatalf("pair %s -> %s was split across lines (or failed): %v", ip, dst, r)
				}
				for k := range r {
					got[dst], lines[k] = k, true
				}
			}
			if got["203.0.113.10"] != got["203.0.113.11"] {
				split++
			}
		}
		if !lines["wan1"] || !lines["wan2"] {
			t.Fatalf("20 pairs all landed on one line: %v", lines)
		}
		if split == 0 {
			t.Fatal("no client used different lines for different destinations: that is source stickiness, not source+destination")
		}
		t.Logf("%d of 10 clients reached the two destinations over different lines", split)
		in.Cfg.Policies[0].Affinity = "connection"
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a router-only rule pins its traffic while LAN traffic stays balanced", func(t *testing.T) {
		in.Cfg.Policies = append(in.Cfg.Policies, config.Policy{Name: "wan1_only", Members: []string{"wan1"},
			Affinity: config.AffinityConnection, AffinityTimeout: 300, UnavailableAction: config.UnavailableReject})
		in.Cfg.Rules = []config.Rule{{Name: "pin_router", Enabled: true, Priority: 10, Local: true, Proto: "tcp",
			DestPort: []string{"80"}, Policy: "wan1_only"}}
		in.Shares["wan1_only"] = map[int]map[int]int{4: {1: 100}}
		in.Shares["balanced"][4] = map[int]int{1: 50, 2: 50}
		if _, err := ap.Apply(in); err != nil {
			t.Fatal(err)
		}
		if r := tp.Count("", 60); r["wan1"] != 60 {
			t.Fatalf("router traffic should be pinned to wan1: %v", r)
		}
		r := tp.Count("lan", 200)
		nstest.Within(t, "lan wan1 share", r["wan1"], 200, 0.50, 0.12)
		// the pinned line fails: the router's traffic is rejected, not moved to the other line
		in.Shares["wan1_only"][4] = map[int]int{}
		if err := ap.UpdateWeights(in); err != nil {
			t.Fatal(err)
		}
		if r := tp.Count("", 3); r["wan1"]+r["wan2"] != 0 {
			t.Fatalf("pinned traffic leaked to another line: %v", r)
		}
	})

	t.Run("remove withdraws only swanix objects", func(t *testing.T) {
		if err := ap.Remove(in); err != nil {
			t.Fatal(err)
		}
		if out := nstest.Sh(t, "nft", "list", "tables"); strings.Contains(out, " swanix") || !strings.Contains(out, "fw4sim") {
			t.Fatalf("tables after remove:\n%s", out)
		}
		rules := nstest.Sh(t, "ip", "rule", "show")
		if strings.Contains(rules, "fwmark") || strings.Contains(rules, "suppress") {
			t.Fatalf("rules remain:\n%s", rules)
		}
		if out := nstest.Sh(t, "ip", "route", "show", "table", "1001"); strings.TrimSpace(out) != "" {
			t.Fatalf("table 1001 not empty:\n%s", out)
		}
		if got := strings.TrimSpace(nstest.Sh(t, "cat", "/proc/sys/net/ipv4/conf/eth1/rp_filter")); got != "1" {
			t.Fatalf("rp_filter not restored: %s", got)
		}
	})
}
