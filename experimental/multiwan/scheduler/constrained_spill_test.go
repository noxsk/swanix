package scheduler

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
)

// V2 experiment: constrain positive deltas of the ordinary smoothed objective;
// never invent an allocation direction or overwrite the EWMA intent. The Mbps
// scale remains a heuristic, not a byte-rate guarantee for unequal/long flows.
// Production integration is gated by these comparisons and edge-case tests.
type constrainedSpill struct {
	base      *Scheduler
	threshold float64
	saturated map[int]bool
}

func newConstrainedSpill(p Params) *constrainedSpill {
	threshold := p.Saturation
	p.Saturation = 0
	return &constrainedSpill{New(p), threshold, map[int]bool{}}
}

func (s *constrainedSpill) Step(in []Input) Result {
	prev := s.base.applied
	r := s.base.Step(in)
	pool := candidates(in)
	valid := s.threshold > 0 && len(pool) > 0 && len(pool) == len(prev)
	var totalCap float64
	for _, w := range pool {
		_, exists := prev[w.ID]
		valid = valid && exists && w.MetricsFresh && capacity(w) > 0
		totalCap += capacity(w)
	}
	if !valid || s.base.p.Objective != config.ObjectiveCapacity && !comparableMetrics(pool) {
		s.saturated = map[int]bool{}
		return r
	}
	nextSat := map[int]bool{}
	for _, w := range pool {
		u := Utilisation(w)
		if u >= s.threshold || s.saturated[w.ID] && u >= s.threshold-saturationRelease {
			nextSat[w.ID] = true
		}
	}
	s.saturated = nextSat
	if len(nextSat) == 0 || len(nextSat) == len(pool) {
		return r
	}
	// Constraint applies only when the ordinary objective actually unloads a
	// saturated path. A health-driven move in the opposite direction survives.
	unloading := false
	for id := range nextSat {
		unloading = unloading || s.base.applied[id] < prev[id]
	}
	if !unloading {
		return r
	}
	delta := make(map[int]float64, len(pool))
	var gain, loss float64
	for _, w := range pool {
		d := s.base.applied[w.ID] - prev[w.ID]
		if d > 0 {
			if !nextSat[w.ID] {
				room := 100 * capacity(w) * math.Max(0, s.threshold-Utilisation(w)) / totalCap
				d = math.Min(d, room)
			}
			gain += d
		} else {
			loss -= d
		}
		delta[w.ID] = d
	}
	target := make(map[int]float64, len(pool))
	for id, d := range delta {
		if d < 0 && loss > 0 {
			d *= gain / loss
		}
		target[id] = prev[id] + d
	}
	// base.Step already applied deadband to the accumulated intent. Do not
	// apply it again to a limited delta: a persistent 2-point cap must progress.
	// Do not reseed smoothed: the unmet objective remains available next round.
	s.base.applied = target
	r.Shares = roundShares(target, s.base.p.MinShare)
	r.Changed = !sameShares(r.Shares, roundShares(prev, s.base.p.MinShare))
	// Result flags describe unloading donors, not every observed saturated
	// line. Never expose the hysteresis map to caller mutation. A saturated
	// receiver is allowed by the health objective but is not a spilling donor.
	r.Saturated = make(map[int]bool)
	for id := range nextSat {
		if target[id] < prev[id]-1e-9 {
			r.Saturated[id] = true
		}
	}
	return r
}

func TestConstrainedSpillRespectsObjective(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveLatency, config.ObjectiveBalanced, config.ObjectiveThroughput} {
		for _, scenario := range []string{"near-threshold", "degraded", "recovering", "preference"} {
			t.Run(objective+"/"+scenario, func(t *testing.T) {
				p := Params{Objective: objective, Saturation: .9}
				s := newConstrainedSpill(p)
				p.Saturation = 0
				ordinary := New(p)
				in := []Input{loaded(1, 0), loaded(2, 0)}
				for i := range in {
					in[i].BaseRTT, in[i].RTT = 10*time.Millisecond, 10*time.Millisecond
				}
				s.Step(in)
				ordinary.Step(in)
				in[0].RXMbps, in[1].RXMbps = 95, 10
				switch scenario {
				case "near-threshold":
					in[1].RXMbps = 86
				case "degraded":
					in[1].State, in[1].LossRate = health.Degraded, .5
				case "recovering":
					in[1].State, in[1].RecoverRamp = health.Recovering, 30*time.Second
				case "preference":
					in[1].Preference = 1
				}
				var got, want Result
				for i := 0; i < 80; i++ {
					got, want = s.Step(in), ordinary.Step(in)
				}
				t.Logf("constrained=%v ordinary=%v", got.Shares, want.Shares)
				if math.Abs(float64(got.Shares[2]-want.Shares[2])) > 3 {
					t.Fatal("objective suppressed")
				}
				if scenario == "near-threshold" && got.Shares[2] <= 50 {
					t.Fatal("persistent small corrections froze")
				}
			})
		}
	}
}

func TestConstrainedSpillFeedback(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveBalanced} {
		for _, interval := range []int{1, 2, 5} {
			t.Run(fmt.Sprintf("%s/%ds", objective, interval), func(t *testing.T) {
				p := Params{Objective: objective, Saturation: .9}
				hard := runFeedbackModel(t, p, interval, 100, false)
				got := runFeedbackController(t, newConstrainedSpill(p), interval, 100, false, false)
				rev := runFeedbackController(t, newConstrainedSpill(p), interval, 100, true, false)
				fine := runFeedbackController(t, newConstrainedSpill(p), interval, 1000, false, false)
				p.Saturation = 0
				off := runFeedbackModel(t, p, interval, 100, false)
				for i, st := range got {
					p95 := func(v feedbackStats) float64 { return v.delays[len(v.delays)*95/100] }
					t.Logf("%s hard/off/V2 Mbps=%.2f/%.2f/%.2f queue-p95=%.2f/%.2f/%.2f ms; V2 late transitions=%d max jump=%d", feedbackPhases[i].name, hard[i].served/60, off[i].served/60, st.served/60, p95(hard[i]), p95(off[i]), p95(st), st.lateSpillChanges, st.maxJump)
					if !slices.Equal(st.trace, rev[i].trace) {
						t.Fatal("order-dependent")
					}
					if math.Abs(st.served-fine[i].served)/60 > 1 {
						t.Fatal("integration sensitivity")
					}
					if i == 1 || i == 2 {
						if st.served < hard[i].served || st.served < .98*off[i].served {
							t.Fatal("service regression")
						}
						if p95(st) > p95(off[i])*1.1+1 {
							t.Fatal("queue regression vs disabled spill")
						}
					}
				}
				if got[3].endQueue > 1e-6 || got[3].dropped > 1e-6 {
					t.Fatal("failed recovery")
				}
			})
		}
	}
}
