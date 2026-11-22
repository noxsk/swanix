package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

func heterogeneous() []Input {
	a, b := wan(1, 50, 10), wan(2, 500, 100)
	a.MetricsFresh, b.MetricsFresh = true, true
	a.BaseRTT, a.RTT = 10*time.Millisecond, 10*time.Millisecond
	b.BaseRTT, b.RTT = 80*time.Millisecond, 80*time.Millisecond
	return []Input{a, b}
}

func TestObjectiveHeterogeneous(t *testing.T) {
	for _, tc := range []struct {
		objective      string
		size, min, max int
	}{
		{config.ObjectiveCapacity, 256, 8, 10},
		{config.ObjectiveLatency, 256, 97, 99},
		{config.ObjectiveThroughput, 256, 8, 10},
		{config.ObjectiveBalanced, 16, 95, 99},
		{config.ObjectiveBalanced, 16384, 1, 5},
	} {
		t.Run(fmt.Sprintf("%s/%dKiB", tc.objective, tc.size), func(t *testing.T) {
			r := New(Params{Objective: tc.objective, FlowSizeKiB: tc.size}).Step(heterogeneous())
			if r.Shares[1] < tc.min || r.Shares[1] > tc.max || total(r.Shares) != 100 {
				t.Fatal(r.Shares)
			}
			t.Logf("low-latency/low-capacity vs high-latency/high-capacity: %v", r.Shares)
		})
	}
}

func TestObjectiveLatencyCountsQueueWithoutChangingHealth(t *testing.T) {
	in := heterogeneous()
	s := New(Params{Objective: config.ObjectiveLatency})
	before := s.Step(in)
	in[0].RTT = 300 * time.Millisecond
	in[0].RXMbps = 45
	var after Result
	for i := 0; i < 80; i++ {
		after = s.Step(in)
	}
	if after.Shares[1] >= before.Shares[1] || after.Shares[1] > 5 {
		t.Fatalf("%v -> %v", before.Shares, after.Shares)
	}
	if in[0].State != health.Healthy {
		t.Fatal("suitability changed health")
	}
}

func TestObjectiveWholePoolFallback(t *testing.T) {
	for _, objective := range []string{config.ObjectiveLatency, config.ObjectiveBalanced, config.ObjectiveThroughput} {
		for _, missing := range []string{"fresh", "baseline", "rtt"} {
			in := heterogeneous()
			in[0].RXMbps = 49
			switch missing {
			case "fresh":
				in[1].MetricsFresh = false
			case "baseline":
				in[1].BaseRTT = 0
			case "rtt":
				in[1].RTT = 0
			}
			r := New(Params{Objective: objective, LatencyReservePercent: 50}).Step(in)
			if r.Shares[1] != 9 || r.Shares[2] != 91 {
				t.Fatalf("%s %s: %v", objective, missing, r.Shares)
			}
			if r.Reasons[1] != "objective fallback: incomplete fresh load/RTT evidence" {
				t.Fatal(r.Reasons)
			}
		}
	}
}

func TestObjectiveStaleCannotReceiveImmediateSpill(t *testing.T) {
	for _, objective := range []string{config.ObjectiveLatency, config.ObjectiveThroughput, config.ObjectiveBalanced} {
		in := heterogeneous()
		in[0].RXMbps = 49
		in[1].MetricsFresh = false
		s := New(Params{Objective: objective, Saturation: .9})
		r := s.Step(in)
		if r.Shares[1] != 9 || len(r.Saturated) != 0 {
			t.Fatalf("%s: unknown path received spill: %+v", objective, r)
		}
		in[1].MetricsFresh = true
		if r = s.Step(in); r.Shares[1] < 1 || r.Shares[1] > 20 || !r.Saturated[1] {
			t.Fatalf("%s: fresh spare capacity did not receive spill: %+v", objective, r)
		}
	}
}

func TestObjectiveReserveAndFallback(t *testing.T) {
	in := heterogeneous()
	before := New(Params{Objective: config.ObjectiveThroughput}).Step(in)
	after := New(Params{Objective: config.ObjectiveThroughput, LatencyReservePercent: 50}).Step(in)
	if after.Shares[1] >= before.Shares[1] || after.Shares[1] < 1 {
		t.Fatalf("reserve %v -> %v", before.Shares, after.Shares)
	}
	// When baseline RTT is effectively equal, no path merits a reserve.
	in[1].BaseRTT, in[1].RTT = in[0].BaseRTT, in[0].RTT
	equal := New(Params{Objective: config.ObjectiveThroughput, LatencyReservePercent: 50}).Step(in)
	if !sameShares(equal.Shares, before.Shares) {
		t.Fatal(equal.Shares)
	}
	// Do not strand capacity or let backup reservation interfere with failover.
	in[1].State = health.Down
	lone := New(Params{Objective: config.ObjectiveThroughput, LatencyReservePercent: 80}).Step(in)
	if lone.Shares[1] != 100 {
		t.Fatal(lone.Shares)
	}
}

func TestObjectivesPreserveSchedulerInvariants(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveLatency, config.ObjectiveThroughput, config.ObjectiveBalanced} {
		s := New(Params{Objective: objective, Saturation: .9})
		in := heterogeneous()
		initial := s.Step(in)
		in[0].RXMbps = 49
		spilled := s.Step(in)
		if !spilled.Saturated[1] || absShareDelta(spilled.Shares[1], initial.Shares[1]) > 11 {
			t.Fatalf("%s spill %v", objective, spilled)
		}
		in[0].RXMbps = 0
		prev := spilled.Shares[1]
		for i := 0; i < 50; i++ {
			r := s.Step(in)
			if r.Shares[1]-prev > 11 || total(r.Shares) != 100 || r.Shares[1] < 1 || r.Shares[2] < 1 {
				t.Fatalf("%s release %v", objective, r)
			}
			prev = r.Shares[1]
		}
		in[0].State = health.Down
		if r := s.Step(in); r.Shares[2] != 100 || len(r.Shares) != 1 {
			t.Fatal(r.Shares)
		}
		in[1].Role = config.RoleBackup
		if r := s.Step(in); r.Shares[2] != 100 {
			t.Fatal(r.Shares)
		}
	}
}

func TestObjectiveEqualPathsStableAndOrderIndependent(t *testing.T) {
	for _, objective := range []string{config.ObjectiveLatency, config.ObjectiveThroughput, config.ObjectiveBalanced} {
		in := heterogeneous()
		in[1] = in[0]
		in[1].ID = 2
		s := New(Params{Objective: objective})
		for i := 0; i < 100; i++ {
			in[0], in[1] = in[1], in[0]
			// Small RTT measurement noise should not cause kernel churn.
			in[0].RTT = time.Duration(10000+i%3) * time.Microsecond
			r := s.Step(in)
			if r.Shares[1] != 50 || r.Shares[2] != 50 || i > 0 && r.Changed {
				t.Fatal(r)
			}
		}
	}
}

func TestObjectiveFactorsAndDirection(t *testing.T) {
	for _, objective := range []string{config.ObjectiveLatency, config.ObjectiveThroughput, config.ObjectiveBalanced} {
		for _, bias := range []string{config.BiasDownload, config.BiasUpload, config.BiasBalanced} {
			in := heterogeneous()
			in[1] = in[0]
			in[1].ID = 2
			in[0].Bias, in[1].Bias = bias, bias
			if bias == config.BiasUpload {
				in[0].TXMbps = 9
			} else {
				in[0].RXMbps = 45
			}
			r := New(Params{Objective: objective}).Step(in)
			if r.Shares[1] >= 50 {
				t.Fatalf("%s %s ignored directional load: %v", objective, bias, r.Shares)
			}
		}
		for _, factor := range []string{"loss", "preference", "degraded", "recovering"} {
			in := heterogeneous()
			in[1] = in[0]
			in[1].ID = 2
			switch factor {
			case "loss":
				in[0].LossRate = .4
			case "preference":
				in[0].Preference = 50
			case "degraded":
				in[0].State = health.Degraded
			case "recovering":
				in[0].State = health.Recovering
				in[0].RecoverRamp = 30 * time.Second
			}
			r := New(Params{Objective: objective}).Step(in)
			if r.Shares[1] >= 50 {
				t.Fatalf("%s ignored %s: %v", objective, factor, r.Shares)
			}
		}
	}
}

func BenchmarkObjectives(b *testing.B) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveLatency, config.ObjectiveThroughput, config.ObjectiveBalanced} {
		b.Run(objective, func(b *testing.B) {
			s := New(Params{Objective: objective})
			in := heterogeneous()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s.Step(in)
			}
		})
	}
}
