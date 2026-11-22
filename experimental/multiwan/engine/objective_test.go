package engine

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

func TestSchedulePublishesDistinctWorkloadPolicies(t *testing.T) {
	r := newRig(t, true)
	cfg := r.e.o.Config
	cfg.Policies = []config.Policy{
		{Name: "interactive", Members: []string{"wan1", "wan2"}, Objective: config.ObjectiveLatency},
		{Name: "bulk", Members: []string{"wan1", "wan2"}, Objective: config.ObjectiveThroughput, LatencyReservePercent: 50},
	}
	if err := r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for i, ws := range r.e.wans {
		ws.w.TrafficBias = config.BiasDownload
		ws.w.DownloadMbps = []float64{50, 500}[i]
		ws.sampledAt = r.clk.now()
		fs := ws.fams[4]
		fs.m.Force(r.clk.now(), health.Healthy, "test")
		fs.rtt = []time.Duration{10 * time.Millisecond, 80 * time.Millisecond}[i]
		fs.probedAt = r.clk.now()
		fs.lastRoundAt, fs.lastRoundOK = r.clk.now(), true
		var targets []string
		for _, target := range fs.prober.Targets {
			targets = append(targets, target.Raw)
		}
		fs.lastSuccessTargets = targetSignature(targets)
		round := health.RoundResult{OK: true, Targets: []health.TargetResult{{OK: true, RTT: fs.rtt}}}
		fs.q.Push(round)
		fs.objectiveQuality = health.NewQuality(60)
		fs.objectiveQuality.Push(round)
	}
	if err := r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	if r.e.shares["interactive"][4][1] < 95 || r.e.shares["bulk"][4][1] > 5 {
		t.Fatal(r.e.shares)
	}
	snap := r.e.picks.Load()
	if snap.policies["interactive"][4].candidates[0].end != r.e.shares["interactive"][4][1] {
		t.Fatal("proxy snapshot did not receive policy weights")
	}
	st := r.e.Status()
	for _, w := range st.WANs {
		if w.Families[0].State != "healthy" {
			t.Fatal("high RTT incorrectly changed health")
		}
		if w.Families[0].SchedulingReason["interactive"] == "" {
			t.Fatal("missing scheduling explanation")
		}
	}
}

func TestPreviewObjectiveUsesCapacityUntilProbed(t *testing.T) {
	r := newRig(t, false)
	cfg := r.e.o.Config
	for i := range cfg.Policies {
		cfg.Policies[i].Objective = config.ObjectiveLatency
	}
	in, _, err := Preview(cfg, r.nf)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.e.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err = r.e.Schedule(); err != nil {
		t.Fatal(err)
	}
	st := r.e.Status()
	if st.WANs[0].Families[0].SchedulingReason["balanced"] != "objective fallback: incomplete fresh load/RTT evidence" {
		t.Fatal("fallback missing from status", st.WANs[0].Families[0].SchedulingReason)
	}
	for name, fams := range in.Shares {
		for family, shares := range fams {
			for id, share := range shares {
				if r.e.shares[name][family][id] != share {
					t.Fatalf("preview=%v live=%v", in.Shares, r.e.shares)
				}
			}
		}
	}
}
