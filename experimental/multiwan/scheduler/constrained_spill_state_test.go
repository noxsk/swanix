package scheduler

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/config"
)

func TestConstrainedSpillResultDoesNotMutateHysteresis(t *testing.T) {
	a, b := newConstrainedSpill(Params{Saturation: .9}), newConstrainedSpill(Params{Saturation: .9})
	in := []Input{loaded(1, 0), loaded(2, 0)}
	a.Step(in)
	b.Step(in)
	in[0].RXMbps, in[1].RXMbps = 95, 10
	var flagged bool
	for i := 0; i < 10; i++ {
		x, y := a.Step(in), b.Step(in)
		if !sameShares(x.Shares, y.Shares) {
			t.Fatal("controllers diverged")
		}
		if len(x.Saturated) > 0 {
			flagged = true
			clear(x.Saturated)
			in[0].RXMbps = 85 // inside hysteresis band
			break
		}
	}
	if !flagged {
		t.Fatal("fixture did not exercise flags")
	}
	for i := 0; i < 5; i++ {
		x, y := a.Step(in), b.Step(in)
		if !sameShares(x.Shares, y.Shares) || a.saturated[1] != b.saturated[1] {
			t.Fatal("caller mutated controller state")
		}
	}
}

func TestConstrainedSpillSubPercentProgress(t *testing.T) {
	s := newConstrainedSpill(Params{Objective: config.ObjectiveThroughput, Saturation: .9})
	in := []Input{loaded(1, 0), loaded(2, 0)}
	for i := range in {
		in[i].BaseRTT, in[i].RTT = 10*time.Millisecond, 10*time.Millisecond
	}
	s.Step(in)
	in[0].RXMbps, in[1].RXMbps = 100, 89.8 // .1-point receiver cap
	unchangedFractional := false
	for i := 0; i < 100; i++ {
		prev := s.base.applied[2]
		r := s.Step(in)
		if s.base.applied[2] > prev && !r.Changed {
			unchangedFractional = true
		}
		if total(r.Shares) != 100 {
			t.Fatal(r)
		}
	}
	if !unchangedFractional || s.base.applied[2] < 52 {
		t.Fatal("fractional progress lost", s.base.applied, unchangedFractional)
	}
	t.Logf("100 samples with 0.1-point cap: fractional shares=%v", s.base.applied)
}
