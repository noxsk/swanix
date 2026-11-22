package scheduler

import (
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/health"
	"testing"
	"time"
)

func absShareDelta(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

func TestSoftSpillPreservesQualityAndRecovery(t *testing.T) {
	for _, scenario := range []string{"degraded", "recovering", "preference"} {
		t.Run(scenario, func(t *testing.T) {
			s := New(Params{Saturation: .9})
			in := []Input{loaded(1, 0), loaded(2, 0)}
			prev := s.Step(in)
			in[0].RXMbps, in[1].RXMbps = 95, 10
			switch scenario {
			case "degraded":
				in[1].State, in[1].LossRate = health.Degraded, .5
			case "recovering":
				in[1].State, in[1].RecoverRamp = health.Recovering, 30*time.Second
			case "preference":
				in[1].Preference = 1
			}
			for i := 0; i < 40; i++ {
				r := s.Step(in)
				if total(r.Shares) != 100 || absShareDelta(r.Shares[1], prev.Shares[1]) > 11 {
					t.Fatal(r)
				}
				prev = r
			}
			if prev.Shares[2] > 60 {
				t.Fatal("saturation erased poor receiver quality", prev)
			}
			t.Logf("receiver %s final shares=%v", scenario, prev.Shares)
		})
	}
}

func TestSoftSpillDoesNotExposeHysteresis(t *testing.T) {
	s := New(Params{Saturation: .9})
	r := s.Step([]Input{loaded(1, 95), loaded(2, 10)})
	clear(r.Saturated)
	r = s.Step([]Input{loaded(1, 85), loaded(2, 10)})
	if !r.Saturated[1] {
		t.Fatal("caller modified hysteresis")
	}
}

func TestSoftSpillTargetDiscountBound(t *testing.T) {
	for _, objective := range []string{config.ObjectiveCapacity, config.ObjectiveLatency, config.ObjectiveThroughput, config.ObjectiveBalanced} {
		for _, rx := range []float64{90, 95, 100, 120} {
			in := []Input{loaded(1, rx), loaded(2, 10)}
			for i := range in {
				in[i].BaseRTT, in[i].RTT = 10*time.Millisecond, 10*time.Millisecond
			}
			s := New(Params{Objective: objective, Saturation: .9, Alpha: 1})
			r := s.Step(in)
			// Relative target score ratio may be discounted, never erased or amplified.
			originalRatio := r.Scores[1] / r.Scores[2]
			// Before minimum-share flooring, which can raise a tiny ratio.
			ratio := s.smoothed[1] / s.smoothed[2]
			if ratio < .5*originalRatio-1e-8 || ratio > originalRatio+1e-8 {
				t.Fatal(objective, rx, ratio, originalRatio)
			}
		}
	}
}
