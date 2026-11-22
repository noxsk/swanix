package scheduler

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

func TestCapacitySpillRequiresFreshDestinationLoad(t *testing.T) {
	s := New(Params{Saturation: .9})
	in := []Input{loaded(1, 95), loaded(2, 10)}
	if r := s.Step(in); !r.Saturated[1] || r.Shares[1] >= 50 {
		t.Fatal("fresh destination should allow spill", r)
	}
	in[1].MetricsFresh = false
	if r := s.Step(in); len(r.Saturated) != 0 || len(s.saturated) != 0 {
		t.Fatal("unknown destination load must clear spill and hysteresis", r)
	}
	// Still inside the hysteresis band: stale evidence must not latch the
	// previously saturated state when fresh observations resume.
	in[0].RXMbps = 85
	in[1].MetricsFresh = true
	if r := s.Step(in); len(r.Saturated) != 0 {
		t.Fatal("stale saturation survived incomplete telemetry", r)
	}
	in[0].RXMbps = 95
	if r := s.Step(in); !r.Saturated[1] {
		t.Fatal("fresh saturation should resume spill", r)
	}
}

func TestSaturatedFloorSurvivesShareRepair(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveLatency, config.ObjectiveThroughput, config.ObjectiveBalanced} {
		t.Run(objective, func(t *testing.T) {
			// Preserve the old hard-floor repair regression against its frozen reference.
			s := newHardSpillReference(Params{Objective: objective, Saturation: .9, Alpha: 1})
			in := []Input{loaded(1, 95), loaded(2, 10), loaded(3, 10), loaded(4, 10)}
			for i := range in {
				in[i].BaseRTT, in[i].RTT = 10*time.Millisecond, 10*time.Millisecond
			}
			prev := s.Step(in)
			if prev.Shares[1] != 1 || !prev.Saturated[1] {
				t.Fatalf("initial spill: %+v", prev)
			}
			in[1].Preference, in[2].Preference = 1, 1
			for i := 0; i < 20; i++ {
				next := s.Step(in)
				if next.Shares[1] != 1 || !next.Saturated[1] || total(next.Shares) != 100 {
					t.Fatalf("step %d: repair sent traffic back to saturated line: %+v", i, next)
				}
				for id, share := range next.Shares {
					if d := share - prev.Shares[id]; d > 11 || d < -11 {
						t.Fatalf("step bound exceeded: %v -> %v", prev.Shares, next.Shares)
					}
				}
				prev = next
			}
		})
	}
}
